# Load balancer w Go

Mały, poprawny HTTP reverse round-robin load balancer.

## Założenia

- **HTTP, nie TCP/L4** — balansowanie na poziomie warstwy aplikacji.
- **Balansowanie na requestcie**, nie na połączeniu TCP — każdy request jest niezależnie kierowany do backendu.
- **Algorytm: round-robin** jako wersja podstawowa.
- **Backend może być chwilowo niedostępny** — dlatego health check i pomijanie niezdrowych backendów.
- **Health check**: okresowe `GET /healthz` na każdym backendzie, z krótkim timeoutem.
- **Request timeout i transport z connection poolingiem** — współdzielony `http.Transport`, świadome wartości timeoutów (dial, response header, idle).
- **Brak retry dla metod nieidempotentnych** (`POST`, `PUT`, `PATCH`) — retry tylko dla `GET`/`HEAD`/`OPTIONS`, `maxAttempts = 2`, kolejna próba omija padający backend.
- **Konfiguracja przez flagi lub plik** — lista backendów, adres listen, interwał health checka.
- **Graceful shutdown po `SIGTERM`/`SIGINT`** — dokończenie trwających requestów, zatrzymanie health checker'a.

## Błędy i retry

- brak zdrowego backendu → `503 Service Unavailable`,
- błąd połączenia z backendem → `502 Bad Gateway`,
- timeout backendu (dial/TLS/nagłówki) → `504 Gateway Timeout`,
- błędy upstreamu zwracane przez backend (np. `500`) są przekazywane wprost do klienta bez retry.

Retry dotyczy wyłącznie metod idempotentnych (`GET`, `HEAD`, `OPTIONS`): maksymalnie 2 próby, wykonywane tylko gdy błąd nastąpił przed wysłaniem jakichkolwiek bajtów do klienta (odpowiedź jest streamowana, bez buforowania w pamięci). Metody z efektami ubocznymi (`POST`, `PUT`, `PATCH`) nigdy nie są ponawiane, aby nie duplikować skutków.

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

## Zakres

W zakresie: proxy HTTP, round-robin, health checki, timeouty, retry dla metod idempotentnych, graceful shutdown, metryki Prometheus, testy.

## Świadome ograniczenia

- **Domyślnie round-robin bez wag** — wagi są dostępne (`WithWeight` + `AlgorithmWeightedRoundRobin`), ale nie są domyślne; nie ma weighted least-connections (waga nie wpływa na least-connections).
- **Brak dynamicznej konfiguracji** — lista backendów jest stała w trakcie działania procesu; zmiana wymaga restartu.
- **Retry tylko dla metod idempotentnych i tylko przed wysłaniem bajtów do klienta** — błędy w trakcie streamowania odpowiedzi nie są ponawiane, metody z efektami ubocznymi (`POST`/`PUT`/`PATCH`) nigdy.
- **Brak retry „w ciemno" w `ErrorHandler`** — retry jest realizowane na poziomie serwera, gdzie wiadomo, czy odpowiedź dotarła już do klienta.
- **Health check z progiem, ale bez circuit breakera** — hysteresis (`failureThreshold=3`, `successThreshold=2`) ogranicza flapping, ale nie ma pełnego otwierania/zamykania obwodu.
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
go run ./cmd/lb -backends http://localhost:9001,http://localhost:9002 -listen :8080
```

## Testy

```bash
go test ./...
go test -race ./...
go vet ./...
```
