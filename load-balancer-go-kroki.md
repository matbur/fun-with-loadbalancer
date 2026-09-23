# Load balancer w Go — kroki do wykonania

Checklist do zaimplementowania poprawnego HTTP reverse round-robin load balancera.

## 0. Założenia (opisz w README)

- [ ] HTTP (nie TCP/L4), balansowanie na poziomie requestu (nie połączenia TCP)
- [ ] Algorytm: round-robin
- [ ] Backend może być chwilowo niedostępny → health check `GET /healthz`
- [ ] Timeout requestu + transport z connection poolingiem
- [ ] Brak retry dla metod nieidempotentnych (`POST`/`PUT`/`PATCH`)
- [ ] Konfiguracja przez flagi lub plik
- [ ] Graceful shutdown po `SIGTERM`/`SIGINT`

## 1. Szkielet projektu

- [ ] Utwórz strukturę:

```text
cmd/lb/main.go
internal/config/config.go
internal/balancer/pool.go
internal/balancer/backend.go
internal/proxy/proxy.go
internal/health/checker.go
```

- [ ] Zdefiniuj model danych:

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

- [ ] Sparsuj URL backendu i zwróć błąd, jeśli parsowanie się nie powiedzie
- [ ] Utwórz `httputil.NewSingleHostReverseProxy` raz na backend (nie na request)
- [ ] Dostosuj `Director`/`Rewrite` — ustawiaj `X-Forwarded-Host`, `X-Forwarded-Proto`; nie ufaj klientowskim `X-Forwarded-*`
- [ ] Ustaw `ErrorHandler` zwracający `502 Bad Gateway`
- [ ] Napisz handler zwracający `503`, gdy `pool.Next()` zwróci `nil`

## 3. Lista backendów + round-robin

- [ ] Wczytaj listę backendów z flag/pliku
- [ ] Zaimplementuj `Pool.Next()` z atomowym licznikiem (`atomic.Uint64`) — bez zwykłego `pool.next++`
- [ ] Iteruj po wszystkich backendach od pozycji startowej i zwróć pierwszy zdrowy
- [ ] Zabezpiecz pustą listę (modulo przez zero)

## 4. Pomijanie niezdrowych backendów

- [ ] Flaga `Healthy` per backend (`atomic.Bool`)
- [ ] `Next()` pomija backendy z `Healthy == false`

## 5. Health checker

- [ ] Pętla z `time.Ticker` + `select` na `ctx.Done()` (bez nowej goroutine na tick)
- [ ] Krótki timeout na pojedynczy check
- [ ] `GET /healthz`, odczytaj max 4 KiB body, zamknij body
- [ ] Aktualizuj `Healthy` bez blokowania obsługi requestów
- [ ] Progi: `failureThreshold = 3`, `successThreshold = 2`, osobne liczniki (hysteresis przeciw flappingowi)
- [ ] Checker zamykany przez kontekst

## 6. Transport i timeouty

- [ ] Jeden współdzielony `http.Transport` (nie `http.Client` na request)
- [ ] `Dialer`: `Timeout: 2s`, `KeepAlive: 30s`
- [ ] `MaxIdleConns: 100`, `MaxIdleConnsPerHost: 20`, `MaxConnsPerHost: 100`, `IdleConnTimeout: 90s`
- [ ] `TLSHandshakeTimeout: 2s`, `ResponseHeaderTimeout: 5s`, `ExpectContinueTimeout: 1s`
- [ ] Przypisz transport do proxy
- [ ] Nie ustawiaj jednego globalnego timeoutu na całą odpowiedź

## 7. Błędy i retry

- [ ] Brak backendu → `503 Service Unavailable`
- [ ] Błąd połączenia → `502 Bad Gateway`
- [ ] Timeout backendu → `504 Gateway Timeout`
- [ ] Retry tylko dla `GET`/`HEAD` (i ewentualnie `OPTIONS`), max 2 próby, z omijaniem padającego backendu — albo jawnie zadeklaruj brak retry w README

## 8. Graceful shutdown

- [ ] `http.Server` z `ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`, `IdleTimeout`
- [ ] `signal.NotifyContext` na `SIGINT`/`SIGTERM`
- [ ] `server.Shutdown` z timeoutem (np. 10s) w osobnej goroutine
- [ ] Zatrzymanie health checker'a (anulowanie kontekstu)
- [ ] Zamknięcie idle connections transportu

## 9. Testy

- [ ] Używaj `httptest.Server` zamiast prawdziwych procesów
- [ ] Round-robin rozdziela requesty po równo
- [ ] Pusta pula → `503`
- [ ] Niezdrowy backend jest pomijany
- [ ] Backend wraca po ponownym health checku
- [ ] Błąd backendu → `502`
- [ ] Timeout backendu nie zawiesza LB
- [ ] Równoległe requesty bez race condition
- [ ] Graceful shutdown kończy pracę
- [ ] Nagłówki i ścieżka przekazywane poprawnie
- [ ] Uruchom: `go test ./...`, `go test -race ./...`, `go vet ./...`, `go test -run TestLoadBalancer -count=50 ./...`

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
