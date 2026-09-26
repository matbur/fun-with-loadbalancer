# Review całego projektu — load balancer (Go)

**Zakres:** całe repozytorium — przegląd wykonany na commicie `d252444` („12").
**Baseline:** `go vet ./...` oraz `go test -race ./...` — zielone we wszystkich pakietach.

## Mniejsze problemy

### 1. [niski] TOCTOU w `admin.add` — duplikaty URL przy równoczesnych POST

`cmd/lb/admin.go:75–80`: unikalność URL sprawdzana jest na **niezablokowanej kolce** `pool.Backends()`, a `AddBackend` deduplikuje wyłącznie po **wskaźniku** (`pool.go:97`). Dwa równoczesne `POST /backends` z tym samym URL przechodzą pre-check, tworzą dwa różne backendy i oba `AddBackend` się udają → w puli są dwa backendy na ten sam host. Sekwencyjne duplikaty są łapane (409). Realistycznie: tylko lokalne/dev użycie, ale to realna dziura. Fix: dedupe po URL wewnątrz `AddBackend` (pod mutexem) albo mutex serializujący `add`.

### 2. [niski] `admin.remove` — równoczesny DELETE daje 500 zamiast 404

`cmd/lb/admin.go:113–115`: `RemoveBackend` zwraca `ErrBackendNotFound`, mapowany na 500. Przy dwóch równoczesnych DELETE tego samego backendu drugi dostaje 500, choć to błąd klienta. Kosmetyka, ale statusy są już konsekwentne w reszcie API.

### 3. [niski] Nowo dodany backend (POST /backends) dostaje ruch, zanim przejdzie pierwszy health check

`internal/health/checker.go:65–79`: `checkAll` wykonuje się raz przy starcie `Run` i później tylko co interwał (domyślnie 10 s). `NewBackend` ustawia `Healthy = true` (`backend.go:162`). Backend dodany przez admin API jest więc traktowany jako zdrowy i dostaje ruch nawet, jeśli jest martwy — do pierwszego ticka (do 10 s). Circuit breaker ogranicza szkody (po 3 błędach wyklucza), ale przy wadze 1 w małej puli to 10 s 503-ów na część ruchu. Sugestia: kanał powiadamiania checker o dodaniu (natychmiastowe `checkAll`) albo dokumentacja zachowania.

### 4. [niski] Health checki są sekwencyjne — cykl może się rozciągnąć do N×timeout

`checker.go:81–87`: `checkAll` przechodzi po backendach jeden po drugim, każdy ograniczony do `health-check-timeout` (domyślnie 2 s). Przy N martwych/slowach backendach jeden cykl trwa N×2 s >> interwał, tickery się przesuwają, a stan zdrowia robi się nieświeży (backend „zamarły" dłużej pozostaje zdrowy dla proxy, gdyż stan aktualizuje się dopiero po pełnym cyklu). Dla kilku backendów bez znaczenia; przy kilkudziesięciu — warto równoległość z limitem (np. WaitGroup + budget czasu cyklu).

### 5. [niski] Retry może wysłać upstream częściowo zużyte body

`proxy.go:186–219`: retry (GET/HEAD/OPTIONS) po błędzie transportowym używa tego samego `r.Body`. ReverseProxy **strumieniuje** body zamiast go buforować — jeśli błąd zdarzy się w trakcie wysyłki treści (nie przy dialu), retry wyśle obciętą/dokonaną treść. W praktyce GET-y z body to rzadkość, a błędy zwykle zdarzają się na dialu (body nietknięte), więc ryzyko niskie — ale warto to udokumentować jako świadome ograniczenie przy wysyłaniu body metodami retryowalnymi.

---

## Obserwacje (informacyjnie)

6. **healthz >4 KiB nie jest drenowany** (`checker.go:126`) — `io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))` zostawia nieprzeczytane bajty; połączenie nie wraca do puli idle i transport je zamyka (czytany limit jest celowy w checklist, bez wpływu na poprawność). Dla backendów z gadatliwym `/healthz` to po prostu brak reuse'u połączenia.

7. **Duplikaty URL z CLI/pliku nie są odrzucane** — `-backends http://a,http://a` tworzy dwa backendy na ten sam host (podwaja udział w WRR), podczas gdy admin API wymusza unikalność URL. Niespójność między drogami konfiguracji — rozważyć walidację/odrzucenie duplikatów w `config.Parse` (albo dedupe).

8. **Access log pokaże `status=0` dla połączeń upgraded** — ReverseProxy obsługuje upgrade (WebSocket) przez Hijack; `trackingWriter.Hijack()` jest przekazywane dalej, ale `tw.wroteHeader` i `tw.status` zostają 0 → `logAccess` (proxy.go:244) zaloguje `status=0` (metryki pokażą 200 dla `statusWriter`). Dziś nieosiągalne (żaden backend nie upgrade'uje; 12f WebSocket to TODO) — do przemyślenia przy implementacji 12f.

9. **Niespójność `errors.AsType` vs `errors.As`** — `backend.go:106` używa `errors.AsType[*http.MaxBytesError]`, `proxy.go:120–123` `errors.As` + `*http.MaxBytesError`. Funkcjonalnie równoważne (Go 1.27), ale warto ujednolicić.

10. **`/ready` nie uwzględnia `-max-concurrent`** — `pool.Ready()` (`pool.go:76`) zwraca 200, nawet gdy semafor jest pełny. Zamierzone (readiness = dostępność backendów), ale jeśli orchestration ma reagować na przeciążenie — warte przemyślenia.

---

## Co jest solidne (bez zastrzeżeń)

- **Limity (11i):** kolejność body→semafory, fast-path 413 po Content-Length, `MaxBytesError` (chunked) mapowany na 413 i celowo **nie** wywala breakera ani nie triggeruje retry; semafor odmawia 503 bez dotykania backendu, `/live`/`/ready`/`/metrics`/admin poza limitami. Testy pokrywają wszystkie ścieżki (w tym chunked i brak wpływu na breaker).
- **Smooth WRR (nginx):** stan per-backend pod mutexem puli, reset przy `RemoveBackend` i przy niedostępnym backendzie — poprawne i deterministyczne.
- **Circuit breaker:** stany zamknięty→otwarty→półotwarty na atomikach, CAS przejścia open→half-open, „próba po cooldownie" — race-safe; `Ready()` nie mutuje stanu.
- **Metryki:** `statusWriter` nie buforuje body, przekazuje Flusher/Hijacker/Pusher + `Unwrap`; liczniki statusów i in-flight poprawne.
- **Pokrycie testami:** bardzo dobre — pool (RR/WRR/LC, add/remove, konkurencja), breaker (wszystkie przejścia), health (progi, prune, timeout, context), config (walidacja ujemnych wartości, braki), proxy (access log, limity, 413, retry), metryki, admin, healthz, logger. `go test -race ./...` zielone.

---

## Podsumowanie

Wszystkie błędy wykryte w review zostały naprawione (precedencja flag vs plik; wiring graceful shutdown — `serveUntilShutdown` w `cmd/lb/server.go` z testem dokumentującym — oraz domknięcie checklistowego `CloseIdleConnections`). Pozostałe znaleziska to drobiazgi (1–5) i obserwacje (6–10), które nie blokują — większość warta jednego zdania w README albo szybkiego fixu przy okazji.