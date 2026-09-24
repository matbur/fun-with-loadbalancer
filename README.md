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

## Zakres

W zakresie: proxy HTTP, round-robin, health checki, timeouty, graceful shutdown, testy.

Poza zakresem (celowo): WebSocket, retry „w ciemno”, dynamiczna konfiguracja, metryki — chyba że zostanie czas (patrz `load-balancer-go-kroki.md`).

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
