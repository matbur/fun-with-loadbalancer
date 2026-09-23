# Load balancer w Go

Mały, poprawny HTTP reverse round-robin load balancer.

## Założenia

- **HTTP, nie TCP/L4** — balansowanie na poziomie warstwy aplikacji.
- **Balansowanie na requestcie**, nie na połączeniu TCP — każdy request jest niezależnie kierowany do backendu.
- **Algorytm: round-robin** jako wersja podstawowa.
- **Backend może być chwilowo niedostępny** — dlatego health check i pomijanie niezdrowych backendów.
- **Health check**: okresowe `GET /healthz` na każdym backendzie, z krótkim timeoutem.
- **Request timeout i transport z connection poolingiem** — współdzielony `http.Transport`, świadome wartości timeoutów (dial, response header, idle).
- **Brak retry dla metod nieidempotentnych** (`POST`, `PUT`, `PATCH`) — retry tylko dla `GET`/`HEAD`, chyba że specyfikacja mówi inaczej.
- **Konfiguracja przez flagi lub plik** — lista backendów, adres listen, interwał health checka.
- **Graceful shutdown po `SIGTERM`/`SIGINT`** — dokończenie trwających requestów, zatrzymanie health checker'a.

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
