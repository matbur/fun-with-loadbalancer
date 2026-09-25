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
- **Limity** — max rozmiar body requestu (`-max-body-bytes`, domyślnie 1 MiB) i max równoległych requestów (`-max-concurrent`); przekroczenie body → `413`, brak miejsca → `503`.
- **Graceful shutdown po `SIGTERM`/`SIGINT`** — dokończenie trwających requestów, zatrzymanie health checker'a.

## Błędy i retry

- brak zdrowego backendu → `503 Service Unavailable`,
- błąd połączenia z backendem → `502 Bad Gateway`,
- timeout backendu (dial/TLS/nagłówki) → `504 Gateway Timeout`,
- błędy upstreamu zwracane przez backend (np. `500`) są przekazywane wprost do klienta bez retry.

Retry dotyczy wyłącznie metod idempotentnych (`GET`, `HEAD`, `OPTIONS`): maksymalnie 2 próby, wykonywane tylko gdy błąd nastąpił przed wysłaniem jakichkolwiek bajtów do klienta (odpowiedź jest streamowana, bez buforowania w pamięci). Metody z efektami ubocznymi (`POST`, `PUT`, `PATCH`) nigdy nie są ponawiane, aby nie duplikować skutków.

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

### Endpoint `/backends`

`cmd/lb` wystawia admin endpoint (bez autoryzacji — do użytku lokalnego/developerskiego):

- `GET /backends` — lista backendów jako JSON (`url`, `weight`, `healthy`, `active`),
- `POST /backends` z ciałem `{"url": "http://localhost:9003", "weight": 1}` — dodaje backend (`201 Created`; `400` — zły URL/JSON; `409` — duplikat URL),
- `DELETE /backends` z ciałem `{"url": "..."}` — usuwa (`204 No Content`; `404` — brak takiego backendu).

Dodany backend dostaje współdzielony transport oraz te same parametry breakera co backendy startowe; health checker podchwyci go od następnego cyklu.

## Circuit breaker

Obok pasywnego health checka każdy backend ma **circuit breaker** chroniący przed serią błędów transportu (błędy połączenia, timeouty):

- **Domknięty (closed)** — normalny ruch; kolejne błędy transportu liczą się do progu.
- **Otwarty (open)** — po `threshold` (=3) kolejnych błędach backend wypada z rotacji (`Pool.Next()` go pomija) do końca `cooldown` (=5s).
- **Półotwarty (half-open)** — po cooldownie backend wraca jako „próba": pierwszy request decyduje — sukces domyka obwód, błąd otwiera go ponownie z nowym cooldownem.

W przeciwieństwie do health checka (cykliczny `GET /healthz` co 10s) breaker reaguje **natychmiast** na błędy obsługi requestów — backend może wypaść z rotacji, zanim pierwszy cykl checkera to zauważy. Liczone są wyłącznie błędy transportu (te dające `502`/`504`); kody HTTP z backendu (np. `500`) są przekazywane dalej i nie otwierają obwodu.

Parametry ustawia się per backend (`WithBreakerThreshold`, `WithBreakerCooldown`), raportowanie jest automatyczne — proxy karmi breaker sukcesem/błędem każdej próby. Retry i breaker współpracują: jeśli pierwsza próba otworzy obwód, druga próba omija ten backend.

## Endpointy `/live` i `/ready`

Endpointy dla orchestratorów (np. Kubernetes liveness/readiness probes):

- `GET /live` — **liveness**: `200`, dopóki proces działa; nie ma wewnętrznego stanu do sprawdzenia.
- `GET /ready` — **readiness**: `200`, gdy co najmniej jeden backend może przyjąć ruch — jest zdrowy wg health checka *i* ma nieotwarty obwód circuit breakera; `503` w przeciwnym razie (ten sam wynik co odpowiedź proxy „no healthy backends").

`/ready` to **pasywny odczyt** (`Pool.Ready()`): w przeciwieństwie do `Available()` nie powoduje przejścia open → half-open na breakerze — samo sondowanie readiness nie zmienia stanu obwodu (test `TestReadyDoesNotTripBreaker`).

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

- **Body requestu** (`-max-body-bytes`, domyślnie `1 MiB`; `0` = bez limitu): request z deklarowanym `Content-Length` ponad limit dostaje natychmiast `413`. Body o nieznanej długości (chunked) przerywane jest w trakcie przesyłania przez `http.MaxBytesReader`, a błąd `*http.MaxBytesError` mapowany jest na `413` w `StatusCodeForError`. Taki błąd jest **błędem klienta** — nie otwiera circuit breakera i nie wyzwala retry (test `TestServeHTTPBodyTooLargeDoesNotTripBreaker`).
- **Równoległe requesty** (`-max-concurrent`, domyślnie `0` = bez limitu): tokenowy semafor w proxy — przy pełnym limicie nowy request dostaje `503` bez dotykania backendów (access log na poziomie `warn`).

Oba limity dotyczą wyłącznie ruchu proxowanego — endpointy `/metrics`, `/backends`, `/live`, `/ready` im nie podlegają.

## Zakres

W zakresie: proxy HTTP, algorytmy (round-robin, weighted round-robin, least-connections), health checki, circuit breaker, timeouty, retry dla metod idempotentnych, dynamiczne backendy (endpoint `/backends`), limity body i równoległości, graceful shutdown, metryki Prometheus, structured logging, testy.

## Świadome ograniczenia

- **Domyślnie round-robin bez wag** — wagi są dostępne (`WithWeight` + `AlgorithmWeightedRoundRobin`), ale nie są domyślne; nie ma weighted least-connections (waga nie wpływa na least-connections).
- **Endpoint `/backends` bez autoryzacji** — wygodny do demo i lokalnego devu; w produkcji wymagałby uwierzytelnienia i ograniczeń (rate limit, dozwolone sieci).
- **Retry tylko dla metod idempotentnych i tylko przed wysłaniem bajtów do klienta** — błędy w trakcie streamowania odpowiedzi nie są ponawiane, metody z efektami ubocznymi (`POST`/`PUT`/`PATCH`) nigdy.
- **Brak retry „w ciemno" w `ErrorHandler`** — retry jest realizowane na poziomie serwera, gdzie wiadomo, czy odpowiedź dotarła już do klienta.
- **Circuit breaker tylko dla błędów transportu** — progi health checka (`failureThreshold=3`, `successThreshold=2`) i breakera działają niezależnie; kody 5xx z backendu nie otwierają obwodu (są przekazywane dalej).
- **WebSocket/upgrade poza zakresem** — `ReverseProxy` umie obsługiwać upgrade'y, ale zachowanie przy nich nie jest przetestowane ani wspierane.
- **Brak limitu body requestu i limitu równoległych requestów** — obrona przed przeciążeniem opiera się na limitach połączeń wspólnego transportu, nie na semaforze aplikacyjnym.

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
breaker_threshold: 3              # kolejne błędy transportu otwierające obwód
breaker_cooldown: 5s              # czas otwarcia obwodu przed próbą
log_format: text                  # text | json
log_level: info                   # debug | info | warn | error
max_body_bytes: 1048576           # max body requestu w bajtach; 0 = bez limitu
max_concurrent: 0                 # max równoległych requestów; 0 = bez limitu
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
  "max_concurrent": 0
}
```

## Testy

```bash
go test ./...
go test -race ./...
go vet ./...
```
