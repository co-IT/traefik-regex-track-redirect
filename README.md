# Traefik Regex Request and Redirect

Eigenständig lauffähiges Traefik-Middleware-Plugin, das das Verhalten von
`RedirectRegex` erweitert: Wenn eine URL auf den konfigurierten regulären
Ausdruck passt und sich das Redirect-Ziel ändert, ruft das Plugin zuerst einen
externen HTTP-Service auf. Anschließend liefert es den Redirect aus.

## Ablauf

1. Das Plugin bildet die vollständige Request-URL und prüft `regex`.
2. Bei einem Treffer wird `replacement` angewendet.
3. Das Plugin ruft den Tracking-Endpunkt synchron mit der konfigurierten
   HTTP-Methode auf.
4. Nach der Antwort des Dienstes folgt der Redirect. Ohne Treffer wird der
   nächste Handler unverändert aufgerufen.

Wie Traefiks `RedirectRegex` verwendet das Plugin für GET temporär `302` und
permanent `301`. Für andere Methoden werden `307` beziehungsweise `308`
verwendet, damit Methode und Body erhalten bleiben.

## Konfiguration

| Parameter | Erforderlich | Standard | Beschreibung |
| --- | --- | --- | --- |
| `regex` | ja | – | Go-kompatibler regulärer Ausdruck für die vollständige Request-URL |
| `replacement` | nein | leer | Ersetzungswert; Capture Groups sollten als `${1}` geschrieben werden |
| `permanent` | nein | `false` | Aktiviert permanente Redirects |
| `trackEndpoint` | ja | – | Absolute HTTP(S)-URL des externen Tracking-Dienstes |
| `trackMethod` | nein | `POST` | HTTP-Methode für den Tracking-Aufruf: `GET` oder `POST` |
| `trackTimeout` | nein | `2s` | Timeout im Go-Duration-Format, zum Beispiel `500ms` oder `3s` |
| `trackHeaders` | nein | `{}` | Zusätzliche Header für den Tracking-Aufruf |
| `failOnTrackError` | nein | `false` | Liefert bei Fehlern des Tracking-Dienstes `502` statt des Redirects |

Bei `GET` ruft das Plugin `trackEndpoint` unverändert und ohne Request-Body auf.
Es fügt insbesondere keine Query-Parameter hinzu; benötigte Query-Parameter
müssen bereits in `trackEndpoint` konfiguriert sein, zum Beispiel
`https://events.example.com/track?source=traefik`.

Bei `POST` erhält der externe Dienst `Content-Type: application/json` und
folgenden Body:

```json
{
  "timestamp": "2026-08-05T12:00:00Z",
  "plugin": "regexRequestRedirect",
  "method": "GET",
  "sourceUrl": "http://localhost/old/item",
  "redirectUrl": "https://example.com/new/item",
  "statusCode": 302,
  "remoteAddr": "192.0.2.1:1234",
  "userAgent": "Mozilla/5.0"
}
```

Die Antwort des Dienstes gilt bei jedem Status von `200` bis `299` als
erfolgreich. Standardmäßig arbeitet das Plugin *fail-open*: Netzwerkfehler,
Timeouts und andere Statuscodes werden geloggt, verhindern den Redirect aber
nicht. Mit `failOnTrackError: true` arbeitet es *fail-closed*.

## Installation aus einem GitHub-Release

Das Repository muss unter dem in `go.mod` und `.traefik.yml` angegebenen Pfad
`github.com/co-it/traefik-regex-request-and-redirect` veröffentlicht und mit
einem SemVer-Tag versehen sein, zum Beispiel `v1.0.0`.

Statische Traefik-Konfiguration:

```yaml
experimental:
  plugins:
    regexRequestRedirect:
      moduleName: github.com/co-it/traefik-regex-request-and-redirect
      version: v1.0.0
```

Dynamische Konfiguration:

```yaml
http:
  middlewares:
    tracked-redirect:
      plugin:
        regexRequestRedirect:
          regex: ^https?://old\.example\.com/(.*)
          replacement: https://new.example.com/${1}
          permanent: true
          trackEndpoint: https://events.example.com/redirects
          trackMethod: POST
          trackTimeout: 2s
          trackHeaders:
            Authorization: Bearer example-token
          failOnTrackError: false

  routers:
    old-site:
      rule: Host(`old.example.com`)
      middlewares:
        - tracked-redirect
      service: old-site
```

Bei Docker-Labels müssen Dollarzeichen verdoppelt werden (`$${1}`). Secrets
sollten nicht als Klartext im Repository oder in Labels abgelegt werden.

## Lokale Entwicklung mit Traefik

Das Verzeichnis `integration/` startet Traefik mit dem Plugin über
`experimental.localPlugins` sowie je einen Testdienst für Backend und
Tracking:

```sh
docker compose -f integration/docker-compose.yml up -d
bash integration/test.sh
docker compose -f integration/docker-compose.yml down -v
```

## Tests und CI

```sh
go test -race ./...
go vet ./...
```

Die GitHub-Actions-Pipeline prüft Formatierung, `go vet`, Unit-Tests mit
Race-Detector und lädt das Plugin zusätzlich in einem echten Traefik-Container.
Damit wird neben der Go-Implementierung auch die Yaegi-/Traefik-Einbindung
geprüft. Bei einem erfolgreichen Push auf `main` erstellt die Pipeline danach
automatisch ein GitHub-Release. Die Versionsnummer hat das Format
`v1.<run_number>.0`; die GitHub-Run-Number bildet dabei die Minor-Version.

## Datenschutz und Betrieb

Das Ereignis enthält URL, Quell-IP und User-Agent. Betreiber müssen prüfen, ob
diese Daten für ihren Zweck erforderlich sind, angemessen geschützt werden und
den geltenden Aufbewahrungs- und Datenschutzregeln entsprechen. Der Endpoint
ist administrative Konfiguration und sollte nur auf vertrauenswürdige Ziele
zeigen.

## Lizenz

[MIT](LICENSE)
