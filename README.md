# Load balancer w Go

Mały, poprawny HTTP reverse round-robin load balancer.

## Założenia

- **HTTP, nie TCP/L4** — balansowanie na poziomie warstwy aplikacji.
- **Balansowanie na requestcie**, nie na połączeniu TCP — każdy request jest niezależnie kierowany do backendu.
- **Algorytm: round-robin** jako wersja podstawowa.
- **Backend może być chwilowo niedostępny** — dlatego health check, circuit breaker i pomijanie niedostępnych backendów.
- **Health check**: okresowe `GET /healthz` na każdym backendzie, z krótkim timeoutem.
- **Request timeout i transport z connection poolingiem** — współdzielony `http.Transport`, świadome wartości timeoutów (dial, response header, idle).
- **Brak retry dla metod nieidempotentnych** (`POST`, `PUT`, `PATCH`) — retry tylko dla `GET`/`HEAD`/`OPTIONS`, `maxAttempts = 2`, kolejna próba omija padający backend.
- **Konfiguracja przez flagi lub plik YAML/JSON** (`-config`; flagi nadpisują plik) — lista backendów, adres listen, algorytm, wagi, breaker, logowanie, interwały health checka.
- **Structured logging** (`log/slog`) — format `text`/`json` i poziom wybierane flagami; access log per request (method, path, status, duration, backend), odpowiedzi ≥500 jako warn; bez loggera proxy milczy.
- **Limity** — max rozmiar body requestu (`-max-body-bytes`, domyślnie 1 MiB) i max równoległych requestów (`-max-concurrent`); przekroczenie body → `413`, brak miejsca → `503`. Timeouty serwera (`-read-timeout`, `-write-timeout`) domyślnie `0` — limity są rozmiarowe, nie czasowe.
- **Graceful shutdown po `SIGTERM`/`SIGINT`** — dokończenie trwających requestów, zatrzymanie health checker'a.

## Błędy i retry

- brak zdrowego backendu → `503 Service Unavailable`,
- błąd połączenia z backendem → `502 Bad Gateway`,
- timeout backendu (dial/TLS/nagłówki) → `504 Gateway Timeout`,
- błędy upstreamu zwracane przez backend (np. `500`) są przekazywane wprost do klienta bez retry.

Błędy **po stronie klienta** rozróżniane są osobno i nigdy nie są zliczane jako błąd backendu — nie otwierają obwodu, nie wyzwalają retry i nie trafiają do `lb_backend_errors_total`:

- body większe niż `-max-body-bytes` → `413 Request Entity Too Large`,
- body, które nie dotarło w całości (klient się rozłączył, wygasł `-read-timeout`) → `400 Bad Request`, a przy wygasłym terminie `408 Request Timeout`.

Rozróżnienie jest możliwe, bo proxy owija `r.Body` i zapamiętuje błąd odczytu (`trackingBody`). Bez tego `ReverseProxy` zgłasza niedokończone body i zerwane połączenie z backendem tym samym kodem co awarię backendu — a klient, który zrezygnował z wgrywania pliku, potrafił wyjechać obwód zdrowego backendu i wyłączyć cały load balancer (`TestServeHTTPBodyReadFailureDoesNotTripBreaker`).

Retry dotyczy wyłącznie metod idempotentnych (`GET`, `HEAD`, `OPTIONS`): maksymalnie 2 próby, wykonywane tylko gdy błąd nastąpił przed wysłaniem jakichkolwiek bajtów do klienta (odpowiedź jest streamowana, bez buforowania w pamięci). Metody z efektami ubocznymi (`POST`, `PUT`, `PATCH`) nigdy nie są ponawiane, aby nie duplikować skutków. Błędu klienta nie ponawia się w ogóle — body i tak jest już zużyte.

Przed serią błędów transportu chroni dodatkowo circuit breaker — szczegóły w sekcji niżej.

## Nagłówki hop-by-hop

Proxy usuwa nagłówki hop-by-hop (RFC 7230 §6.1) **w obu kierunkach** — robi to automatycznie `httputil.ReverseProxy`:

- z żądania do backendu: `Connection`, `Proxy-Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade` oraz wszystkie nagłówki wymienione w nagłówku `Connection`;
- z odpowiedzi do klienta: ten sam zestaw (w tym nagłówki wskazane przez `Connection` odpowiedzi);

Klienckie `Connection: close` **nie jest przekazywane do backendu** — LB utrzymuje własne persistent connection (zamknięcie połączenia z klientem obsługuje `net/http`).

Wyjątek wynikający z zachowania stdlib: gdy klient zgłosił `TE: trailers`, proxy dodaje `Te: trailers` do żądania backendu (issue 21096), żeby backend wiedział, że wspiera traillery.

Zachowanie jest przypięte testami (`TestServeHTTPStripsHopByHopHeadersFromRequest/FromResponse`, `TestServeHTTPDoesNotForwardConnectionClose`).

## Algorytmy

Domyślnie pula używa **round-robin** (atomowy licznik). Dostępne alternatywy, wybierane przy budowaniu puli (`balancer.NewPool(..., balancer.WithAlgorithm(...))`):

- **Weighted round-robin** (`AlgorithmWeightedRoundRobin`) — smooth weighted round-robin (algorytm nginx): backend z wagą `w` dostaje ok. `w` razy więcej requestów niż backend z wagą 1. Waga ustawiana przez `balancer.WithWeight(w)` w `NewBackend`. Niezdrowe backendy są pomijane, a ich udział rozdzielany proporcjonalnie między zdrowe.
- **Least-connections** (`AlgorithmLeastConnections`) — wybiera zdrowy backend z najmniejszą liczbą requestów w trakcie obsługi (licznik in-flight per backend, zwiększany przez `Acquire`, zwalniany przez `Release` po zakończeniu). Remis rozstrzyga pierwszy backend na liście.

```go
backends := []*balancer.Backend{
    b1, // domyślna waga 1
    b2, // balancer.NewBackend(url2, balancer.WithWeight(3))
}
pool := balancer.NewPool(backends, balancer.WithAlgorithm(balancer.AlgorithmWeightedRoundRobin))
```

## Dynamiczne backendy

Lista backendów nie musi być stała — `Pool` wystawia dwa mutatory, bezpieczne przy jednoczesnym ruchu:

- **`AddBackend(b)`** — dodaje backend do puli; `ErrDuplicateBackend`, gdy już jest w puli.
- **`RemoveBackend(b)`** — usuwa backend i zeruje jego stan smooth weighted round-robin (`ErrBackendNotFound`, gdy go nie ma).

Lista jest trzymana jako jeden atomowy snapshot (`atomic.Value`) i mutowana pod mutexem, więc `Next()` zawsze widzi pełną, niezmienioną listę. Requesty w trakcie obsługi na usuwanym backendzie kończą się normalnie — `Next()` po prostu przestaje go zwracać.

Stan smooth WRR (`current`) mieszka **na backendzie**, więc usunięcie/dodanie nigdy nie przenosi stanu między backendami (nowy backend startuje z zerem) — to celowy wybór, o którym mowa przy 11c.

Health checker iteruje aktualną listę: nowe backendy są sprawdzane od następnego cyklu, a stan checka usuniętego backendu jest czyszczony (`prune`), więc ponownie dodany zaczyna od zera.

Jeden cykl sprawdza backendy **równolegle**, ograniczone do `-health-check-parallelism` (domyślnie `16`, `0` = bez limitu). Bez tego cykl kosztował `len(backends) × health_check_timeout` — zmierzone 1.6s dla 8 wiszących backendów przy timeoutie 200ms. Ponieważ `time.Ticker` gubi tyki, gdy cykl trwa, to po przekroczeniu interwału efektywny interwał stawał się czasem cyklu, a ostatni backend z listy nie był sprawdzany, dopóki wszystkie wcześniejsze nie przekroczyły timeoutu. Domyślne `16` utrzymuje cykl poniżej 10s dla pul do 80 backendów; `MaxConnsPerHost: 100` wspólnego transportu i tak ograniczałoby falę, ale zjadłoby budżet połączeń ruchu proxowanego — dlatego limit jest jawny i konfigurowalny (`TestCheckerCycleCostDoesNotScaleWithPoolSize`).

### Endpoint `/backends`

`cmd/lb` wystawia admin endpoint (bez autoryzacji — do użytku lokalnego/developerskiego):

- `GET /backends` — lista backendów jako JSON (`url`, `weight`, `healthy`, `active`),
- `POST /backends` z ciałem `{"url": "http://localhost:9003", "weight": 1}` — dodaje backend (`201 Created`; `400` — zły URL/JSON; `409` — duplikat URL),
- `DELETE /backends` z ciałem `{"url": "..."}` — usuwa (`204 No Content`; `404` — brak takiego backendu).

Usunięty backend znika też z `/metrics` — `Metrics.ForgetBackend` kasuje serie `lb_backend_*`. Wartości labeli tylko przybywają, więc bez tego rejestry rosłyby w nieskończoność. Nie chroni to przed klientem, który tylko dodaje i nigdy nie usuwa.

Dodany backend dostaje współdzielony transport oraz te same parametry breakera co backendy startowe; health checker podchwyci go od następnego cyklu.

## Circuit breaker

Obok pasywnego health checka każdy backend ma **circuit breaker** chroniący przed serią błędów transportu (błędy połączenia, timeouty):

- **Domknięty (closed)** — normalny ruch; kolejne błędy transportu liczą się do progu.
- **Otwarty (open)** — po `threshold` (=3) kolejnych błędach backend wypada z rotacji (`Pool.Next()` go pomija) do końca `cooldown` (=5s).
- **Półotwarty (half-open)** — po cooldownie backend wraca jako „próba": pierwszy request decyduje — sukces domyka obwód, błąd otwiera go ponownie z nowym cooldownem. W stanie half-open przepuszczany jest **dokładnie jeden** request naraz; kolejne dostają `503` albo trafiają na inny backend, dopóki ten request się nie rozstrzygnie. Jeśli nigdy nie zaraportuje wyniku (odpowiedź przerwana w połowie → `ReverseProxy` rzuca panic, więc ani `ReportSuccess`, ani `ReportFailure` nie zdążą), następny request przejmuje po 30s (`probeTimeout`) — inaczej backend wypadałby z rotacji do restartu procesu.

W przeciwieństwie do health checka (cykliczny `GET /healthz` co 10s) breaker reaguje **natychmiast** na błędy obsługi requestów — backend może wypaść z rotacji, zanim pierwszy cykl checkera to zauważy. Liczone są wyłącznie błędy transportu (te dające `502`/`504`); kody HTTP z backendu (np. `500`) są przekazywane dalej i nie otwierają obwodu.

W drugą stronę: **zdrowy `/healthz` zamyka obwód** (`Backend.ReportHealthy`, wywoływane z health checkera na tej samej histerezie co `Healthy` — 2 kolejne sukcesy). To jedyna droga powrotu dla load balancera bez ruchu: bez requestów nikt nie wygra przejścia open → half-open i nikt nie rozstrzygnie próby, więc obwód zostawałby otwarty do restartu procesu, a `/ready` wskazywałby `503` dla backendu, który odpowiada. Odzyskanie **nie** zeruje licznika błędów przy zamkniętym obwodzie — inaczej backend tracący jeden request na pięć nigdy nie osiągnąłby progu.

Cena tej decyzji: backend, który odpowiada `200` na `/healthz`, ale wywala ruch (np. wyczerpany pool połączeń), ma obwód zamykany przez checker co 2 cykle. Dla takiego backendu breaker przestaje reagować w 3 requestach i reaguje dopiero po ~2 interwałach checkera + 3 requesty.

Parametry ustawia się per backend (`WithBreakerThreshold`, `WithBreakerCooldown`), raportowanie jest automatyczne — proxy karmi breaker sukcesem/błędem każdej próby. Retry i breaker współpracują: jeśli pierwsza próba otworzy obwód, druga próba omija ten backend.

## Endpointy `/live` i `/ready`

Endpointy dla orchestratorów (np. Kubernetes liveness/readiness probes):

- `GET /live` — **liveness**: `200`, dopóki proces działa; nie ma wewnętrznego stanu do sprawdzenia.
- `GET /ready` — **readiness**: `200`, gdy co najmniej jeden backend może przyjąć ruch — jest zdrowy wg health checka *i* jego obwód nie blokuje teraz ruchu; `503` w przeciwnym razie (ten sam wynik co odpowiedź proxy „no healthy backends").

`/ready` to **pasywny odczyt** (`Pool.Ready()`): w przeciwieństwie do `Available()` nie powoduje przejścia open → half-open na breakerze — samo sondowanie readiness nie zmienia stanu obwodu (test `TestPoolReadyIsPassive`). Odczyt uwzględnia cooldown: obwód otwarty, ale już po `cooldown`, **nie** blokuje ruchu, bo następny request zostałby przepuszczony jako próba. Dzięki temu `/ready` nigdy nie przeczy decyzji o routingu. Powrót do `200` po zamknięciu obwodu przychodzi z health checkera (zdrowy `/healthz` zamyka obwód), więc `/ready` może pokazywać `503` do dwóch cykli checkera po otwarciu.

## Logi (structured logging)

Logi oparte na standardowym `log/slog` — format i poziom konfigurowane flagami (`-log-format text|json`, `-log-level debug|info|warn|error`) lub kluczami `log_format`/`log_level` w pliku konfiguracyjnym.

- **Start**: jeden wpis z pełną konfiguracją (`listen`, `algorithm`, liczba backendów, health check, breaker), plus wpis `debug` per backend (`url`, `weight`).
- **Access log**: jeden wpis na request — `method`, `path`, `status`, `duration`, `backend` (i `remote`). Odpowiedzi ≥500 trafiają na poziom `warn`, reszta na `info`.
- **Cykl życia**: `listening`, `shutting down`, błędy serwera.

Przykład z logami JSON:

```bash
go run ./cmd/lb -backends http://localhost:9001,http://localhost:9002 \
  -log-format json
```

```json
{"time":"...","level":"INFO","msg":"starting load balancer","listen":":8080","algorithm":"round-robin","backends":2}
{"time":"...","level":"INFO","msg":"request","method":"GET","path":"/hello","status":200,"duration":123456,"backend":"localhost:9001"}
{"time":"...","level":"WARN","msg":"request failed","method":"GET","path":"/","status":503,"duration":0}
```

Access log pochodzi z proxy przez opcję `WithLogger` — bez niej (np. w testach) proxy nie loguje niczego.

## Limity

- **Body requestu** (`-max-body-bytes`, domyślnie `1 MiB`; `0` = bez limitu): request z deklarowanym `Content-Length` ponad limit dostaje natychmiast `413`. Body o nieznanej długości (chunked) przerywane jest w trakcie przesyłania przez `http.MaxBytesReader`, a błąd `*http.MaxBytesError` rozpoznawany jest w `clientStatusFor` i mapowany na `413`. Taki błąd jest **błędem klienta** — nie otwiera circuit breakera i nie wyzwala retry (test `TestServeHTTPBodyTooLargeDoesNotTripBreaker`).
- **Równoległe requesty** (`-max-concurrent`, domyślnie `0` = bez limitu): tokenowy semafor w proxy — przy pełnym limicie nowy request dostaje `503` bez dotykania backendów (access log na poziomie `warn`).
Oba limity dotyczą wyłącznie ruchu proxowanego — endpointy `/metrics`, `/backends`, `/live`, `/ready` im nie podlegają.

### Timeouty serwera HTTP

`-read-timeout` i `-write-timeout` (plik: `read_timeout` / `write_timeout`) domyślnie wynoszą `0`, czyli **brak deadline**. To celowe: powyższe limity są rozmiarowe, nie czasowe — body ogranicza `-max-body-bytes`, a nie zegar. Bezpiecznej wartości nie ma; każdy deadline krótszy niż czas wgrania `-max-body-bytes` odtwarza problem, a ten czas zależy od klienta. Wcześniej serwer miał zakodowane `ReadTimeout: 5s` obejmujące cały request razem z body, więc klient zsyłający `1 MiB` wolniej niż ~1.7 Mbit/s dostawał urwany request zamiast obiecanego `413` (regresja: `TestSlowUploadIsNotTruncatedByDefault`). Jedyny domyślny limit czasowy to nagłówki (`ReadHeaderTimeout: 5s`) — żaden klient nie zablokuje procesu samym niekończącym się ciałem.

Ustawienie `-read-timeout` przywraca ucinanie transferu, ale błąd odczytu jest wtedy rozpoznawany jako **błąd klienta** (poprzednia sekcja), więc klient dostaje `408`, a obwód backendu zostaje zamknięty (`TestSlowUploadCutOffByReadTimeoutKeepsTheCircuit`). Traktuj tę flagę jako „ile długo klient może wypełniać ciało zanim zostanie odcięty”, nie jako ochronę przed slowloris. Od slowloris i wiszących keep-alive chronią niezależne, zakodowane na stałe `ReadHeaderTimeout: 5s` oraz `IdleTimeout: 60s` — nie da się ich ustawić, bo ograniczają życie połączenia, a nie semantykę pojedynczego requestu.

`-write-timeout` domyślnie `0`, więc długie odpowiedzi (long-polling, duże pobrania) nie są ucinane. Nagłówki od backendu i tak są ograniczone przez transport (`ResponseHeaderTimeout: 5s`), więc ścieżka „backend nie odpowiada” jest chroniona niezależnie. Jeśli ustawisz `-write-timeout` powyżej 30s, podnieś też `probeTimeout` w `internal/balancer/circuit_breaker.go` — wartość jest dobrana heurystycznie, bo nic wyżej już jej nie wyprowadza.

## Zakres

W zakresie: proxy HTTP, algorytmy (round-robin, weighted round-robin, least-connections), health checki, circuit breaker, timeouty, retry dla metod idempotentnych, dynamiczne backendy (endpoint `/backends`), limity body i równoległości, graceful shutdown, metryki Prometheus, structured logging, testy.

## Świadome ograniczenia

- **Domyślnie round-robin bez wag** — wagi są dostępne (`WithWeight` + `AlgorithmWeightedRoundRobin`), ale nie są domyślne; nie ma weighted least-connections (waga nie wpływa na least-connections).
- **Endpoint `/backends` bez autoryzacji** — wygodny do demo i lokalnego devu; w produkcji wymagałby uwierzytelnienia i ograniczeń (rate limit, dozwolone sieci).
- **Retry tylko dla metod idempotentnych i tylko przed wysłaniem bajtów do klienta** — błędy w trakcie streamowania odpowiedzi nie są ponawiane, metody z efektami ubocznymi (`POST`/`PUT`/`PATCH`) nigdy.
- **Brak retry „w ciemno" w `ErrorHandler`** — retry jest realizowane na poziomie serwera, gdzie wiadomo, czy odpowiedź dotarła już do klienta.
- **Circuit breaker tylko dla błędów transportu** — progi health checka (`failureThreshold=3`, `successThreshold=2`) i breakera działają niezależnie; kody 5xx z backendu nie otwierają obwodu (są przekazywane dalej).
- **WebSocket/upgrade poza zakresem** — `ReverseProxy` umie obsługiwać upgrade'y, ale zachowanie przy nich nie jest przetestowane ani wspierane.
- **Brak domyślnego limitu równoległych requestów** — `-max-concurrent` wynosi `0`, więc liczba współbieżnych requestów ogranicza tylko to, co zdolają przepuścić zasoby procesu; jedynym twardym progiem jest `MaxConnsPerHost: 100` transportu, a ten dotyczy połączeń wychodzących do backendów. Limit body jest włączony domyślnie (1 MiB), ale skoro `-read-timeout` to `0`, klient może wgrywać te 1 MiB dowolnie długo.

## Metryki

Endpoint `/metrics` (format Prometheus, protobuf/text) działa na tym samym listenerze co proxy:

- `lb_http_requests_total{method,status}` — liczba requestów wg metody i kodu odpowiedzi,
- `lb_http_request_duration_seconds{method}` — histogram latencji,
- `lb_http_requests_in_flight` — requesty aktualnie w trakcie obsługi,
- `lb_backend_requests_total{backend}` — próby kierowania requestów do backendu (host),
- `lb_backend_errors_total{backend}` — błędy transportu per backend (podstawa `502`/`504`).

Przykład:

```bash
curl http://localhost:8080/metrics
```

## Uruchomienie

```bash
go run ./cmd/lb \
  -backends http://localhost:9001,http://localhost:9002 \
  -listen :8080 \
  -algorithm round-robin            # round-robin | weighted-round-robin | least-connections
  -weights 1,2                      # wagi per backend (opcjonalne; domyślnie 1)
  -breaker-threshold 3              # kolejne błędy transportu otwierające obwód
  -breaker-cooldown 5s              # czas otwarcia obwodu przed próbą
  -log-format text                  # text | json
  -log-level info                   # debug | info | warn | error
  -max-body-bytes 1048576           # max body requestu w bajtach; 0 = bez limitu
  -max-concurrent 0                 # max równoległych requestów; 0 = bez limitu
  -read-timeout 0                   # max czasu na cały request (0 = bez limitu)
  -write-timeout 0                  # max czasu na odpowiedź (0 = bez limitu)
```

Przykład z wagami i least-connections:

```bash
go run ./cmd/lb -backends http://localhost:9001,http://localhost:9002 \
  -algorithm least-connections -weights 1,2 -listen :8080
```

Całą konfigurację można też przenieść do pliku YAML/JSON — patrz niżej.

## Konfiguracja YAML/JSON

Zamiast (albo obok) flag konfiguracja może pochodzić z pliku:

```bash
go run ./cmd/lb -config lb.yaml
```

Zasady łączenia wartości: **jawnie podane flagi mają pierwszeństwo**, potem wartości z pliku, na końcu domyślne. Plik może pomijać pola — wtedy działają domyślne (lub flagi). Format rozpoznawany jest po rozszerzeniu (`.yaml`, `.yml`, `.json`); czasy trwania podaje się jako string („5s", „10s").

Pełny, opisany przykład w YAML (wszystkie pola z komentarzami):

```bash
go run ./cmd/lb -config examples/lb.yaml
```

Zawartość `examples/lb.yaml`:

```yaml
listen: ":8080"
backends:
  - http://localhost:9001
  - http://localhost:9002
algorithm: weighted-round-robin   # round-robin | weighted-round-robin | least-connections
weights: [2, 1]
health_check_every: 10s
health_check_timeout: 2s
health_check_parallelism: 16      # ile backendów jeden cykl sprawdza naraz; 0 = bez limitu
breaker_threshold: 3              # kolejne błędy transportu otwierające obwód
breaker_cooldown: 5s              # czas otwarcia obwodu przed próbą
log_format: text                  # text | json
log_level: info                   # debug | info | warn | error
max_body_bytes: 1048576           # max body requestu w bajtach; 0 = bez limitu
max_concurrent: 0                 # max równoległych requestów; 0 = bez limitu
read_timeout: 0s                  # max czasu na cały request (0 = bez limitu)
write_timeout: 0s                 # max czasu na odpowiedź (0 = bez limitu)
```

`lb.json` (odpowiednik, bez komentarzy — JSON ich nie wspiera):

```json
{
  "listen": ":8080",
  "backends": ["http://localhost:9001", "http://localhost:9002"],
  "algorithm": "least-connections",
  "breaker_threshold": 3,
  "breaker_cooldown": "5s",
  "log_format": "json",
  "log_level": "debug",
  "max_body_bytes": 1048576,
  "max_concurrent": 0,
  "read_timeout": "0s",
  "write_timeout": "0s"
}
```

## Testy

```bash
go test ./...
go test -race ./...
go vet ./...
```
