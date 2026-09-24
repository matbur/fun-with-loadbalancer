# Load balancer w Go — kroki do wykonania

Checklist do zaimplementowania poprawnego HTTP reverse round-robin load balancera.

## 0. Założenia (opisz w README)

- [x] HTTP (nie TCP/L4), balansowanie na poziomie requestu (nie połączenia TCP)
- [x] Algorytm: round-robin
- [x] Backend może być chwilowo niedostępny → health check `GET /healthz`
- [x] Timeout requestu + transport z connection poolingiem
- [x] Brak retry dla metod nieidempotentnych (`POST`/`PUT`/`PATCH`)
- [x] Konfiguracja przez flagi lub plik
- [x] Graceful shutdown po `SIGTERM`/`SIGINT`

## 1. Szkielet projektu

- [x] Utwórz strukturę:

```text
cmd/lb/main.go
internal/config/config.go
internal/balancer/pool.go
internal/balancer/backend.go
internal/proxy/proxy.go
internal/health/checker.go
```

- [x] Zdefiniuj model danych:

```go
type Backend struct {
    URL    *url.URL
    Proxy  *httputil.ReverseProxy
    Healthy atomic.Bool
}

type Pool struct {
    backends atomic.Value // []*Backend
    next     atomic.Uint64
}
```

## 2. Jeden backend + reverse proxy

- [x] Sparsuj URL backendu i zwróć błąd, jeśli parsowanie się nie powiedzie
- [x] Utwórz `httputil.NewSingleHostReverseProxy` raz na backend (nie na request)
- [x] Dostosuj `Director`/`Rewrite` — ustawiaj `X-Forwarded-Host`, `X-Forwarded-Proto`; nie ufaj klientowskim `X-Forwarded-*`
- [x] Ustaw `ErrorHandler` zwracający `502 Bad Gateway`
- [x] Napisz handler zwracający `503`, gdy `pool.Next()` zwróci `nil`

## 3. Lista backendów + round-robin

- [x] Wczytaj listę backendów z flag/pliku
- [x] Zaimplementuj `Pool.Next()` z atomowym licznikiem (`atomic.Uint64`) — bez zwykłego `pool.next++`
- [x] Iteruj po wszystkich backendach od pozycji startowej i zwróć pierwszy zdrowy
- [x] Zabezpiecz pustą listę (modulo przez zero)

## 4. Pomijanie niezdrowych backendów

- [x] Flaga `Healthy` per backend (`atomic.Bool`)
- [x] `Next()` pomija backendy z `Healthy == false`

## 5. Health checker

- [x] Pętla z `time.Ticker` + `select` na `ctx.Done()` (bez nowej goroutine na tick)
- [x] Krótki timeout na pojedynczy check
- [x] `GET /healthz`, odczytaj max 4 KiB body, zamknij body
- [x] Aktualizuj `Healthy` bez blokowania obsługi requestów
- [x] Progi: `failureThreshold = 3`, `successThreshold = 2`, osobne liczniki (hysteresis przeciw flappingowi)
- [x] Checker zamykany przez kontekst

## 6. Transport i timeouty

- [x] Jeden współdzielony `http.Transport` (nie `http.Client` na request)
- [x] `Dialer`: `Timeout: 2s`, `KeepAlive: 30s`
- [x] `MaxIdleConns: 100`, `MaxIdleConnsPerHost: 20`, `MaxConnsPerHost: 100`, `IdleConnTimeout: 90s`
- [x] `TLSHandshakeTimeout: 2s`, `ResponseHeaderTimeout: 5s`, `ExpectContinueTimeout: 1s`
- [x] Przypisz transport do proxy
- [x] Nie ustawiaj jednego globalnego timeoutu na całą odpowiedź

## 7. Błędy i retry

- [x] Brak backendu → `503 Service Unavailable`
- [x] Błąd połączenia → `502 Bad Gateway`
- [x] Timeout backendu → `504 Gateway Timeout`
- [x] Retry tylko dla `GET`/`HEAD` (i ewentualnie `OPTIONS`), max 2 próby, z omijaniem padającego backendu — albo jawnie zadeklaruj brak retry w README

## 8. Graceful shutdown

- [x] `http.Server` z `ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`, `IdleTimeout`
- [x] `signal.NotifyContext` na `SIGINT`/`SIGTERM`
- [x] `server.Shutdown` z timeoutem (np. 10s) w osobnej goroutine
- [x] Zatrzymanie health checker'a (anulowanie kontekstu)
- [x] Zamknięcie idle connections transportu

## 9. Testy

- [x] Używaj `httptest.Server` zamiast prawdziwych procesów
- [x] Round-robin rozdziela requesty po równo
- [x] Pusta pula → `503`
- [x] Niezdrowy backend jest pomijany
- [x] Backend wraca po ponownym health checku
- [x] Błąd backendu → `502`
- [x] Timeout backendu nie zawiesza LB
- [x] Równoległe requesty bez race condition
- [x] Graceful shutdown kończy pracę
- [x] Nagłówki i ścieżka przekazywane poprawnie
- [x] Uruchom: `go test ./...`, `go test -race ./...`, `go vet ./...` oraz `make test-repeat` (50 powtórzeń)

## 10. Kolejność prac

1. [ ] Jeden backend i działający reverse proxy
2. [ ] Lista backendów
3. [ ] Round-robin z atomowym indeksem
4. [ ] Pomijanie niezdrowych backendów
5. [ ] Health checker
6. [ ] Transport i timeouty
7. [ ] Graceful shutdown
8. [ ] Testy równoległe i `go test -race`
9. [ ] Metryki i dokumentacja ograniczeń

Nie zaczynaj od metryk, retry i dynamicznej konfiguracji — najpierw poprawny przepływ requestu.

## 11. Opcjonalnie, jeśli zostanie czas

- [ ] Prometheus metrics (request count, latency, status codes, backend errors)
- [ ] Obsługa nagłówków hop-by-hop / `Connection: close`
- [ ] Weighted round-robin / least-connections
- [ ] Dynamiczne dodawanie/usuwanie backendów
- [ ] Circuit breaker
- [ ] Konfiguracja YAML/JSON
- [ ] Endpointy `/ready` i `/live`
- [ ] Structured logging
- [ ] Limity body requestu i limit równoległych requestów
