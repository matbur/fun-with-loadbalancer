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

1. [x] Jeden backend i działający reverse proxy
2. [x] Lista backendów
3. [x] Round-robin z atomowym indeksem
4. [x] Pomijanie niezdrowych backendów
5. [x] Health checker
6. [x] Transport i timeouty
7. [x] Graceful shutdown
8. [x] Testy równoległe i `go test -race`
9. [x] Dokumentacja ograniczeń (README); metryki celowo oddelegowane do kroku 11 (opcjonalne)

Nie zaczynaj od metryk, retry i dynamicznej konfiguracji — najpierw poprawny przepływ requestu.

## 11. Opcjonalnie, jeśli zostanie czas

Kroki opcjonalne i niezależne — każdy można realizować osobno, w dowolnej kolejności.

### 11a. Metryki Prometheus

- [x] Request count, latency, status codes, backend errors eksponowane przez `/metrics`

### 11b. Nagłówki hop-by-hop

- [x] Obsługa nagłówków hop-by-hop / `Connection: close` — przypięte testami (stdlib `ReverseProxy` usuwa je automatycznie), dokumentacja w README

### 11c. Weighted round-robin / least-connections

- [x] Weighted round-robin (`AlgorithmWeightedRoundRobin`, smooth nginx) i least-connections (`AlgorithmLeastConnections`) jako alternatywy `Pool.Next()`; wagi przez `WithWeight`; dokumentacja w README

### 11d. Dynamiczne dodawanie/usuwanie backendów

- [x] `Pool.AddBackend`/`RemoveBackend` (mutacja listy pod mutexem, atomowy snapshot; błędy `ErrDuplicateBackend`/`ErrBackendNotFound`); stan smooth WRR przeniesiony na `Backend.current` (zerowany przy usunięciu); health checker czyści stan usuniętych backendów; testy (w tym konkurencyjne add/remove) + README

### 11e. Circuit breaker

- [x] Circuit breaker per backend (`circuitBreaker`): closed/open/half-open, otwarcie po `threshold` kolejnych błędach transportu, cooldown → próba (half-open), sukces domyka / błąd otwiera ponownie; `Pool.Next()` pomija backendy z otwartym obwodem (`Available`); opcje `WithBreakerThreshold`/`WithBreakerCooldown`; testy jednostkowe + end-to-end w proxy + README

### 11f. Konfiguracja YAML/JSON

- [x] Konfiguracja YAML/JSON — flaga `-config` (rozszerzenia `.yaml`/`.yml`/`.json`), pola jak flagi (`listen`, `backends`, `weights`, `algorithm`, `breaker_*`, `health_check_*`); precedencja **flagi > plik > domyślne** (`fileConfig` z pointerami + `flag.Visit`); czasy jako stringi; walidacja algorytmu przeniesiona do `config.Parse`; testy jednostkowe (pełny plik, minimalny, nadpisywanie flagami, złe rozszerzenie/zawartość/czas/algorytm) + README

### 11g. Endpointy `/ready` i `/live`

- [ ] Endpointy `/ready` i `/live`

### 11h. Structured logging

- [ ] Structured logging

### 11i. Limity body requestu i limit równoległych requestów

- [ ] Limity body requestu i limit równoległych requestów
