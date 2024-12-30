# Berliner Polizeimeldungen RSS-Feed

Ein Programm, das Polizeimeldungen von der Website der Berliner Polizei scrapt und als RSS-, Atom- und JSON-Feeds bereitstellt.

## Voraussetzungen

- Docker (Compose)

## Installation

1. Klone das Repository:
   ```bash
   git clone <repository-url>
   cd <repository-verzeichnis>
   ```

2. Konfiguriere die App über die Docker Compose

    Hier kannst du z.B. den Port für das RSS Interface setzen

3. Führe die Docker Compose Datei aus

    ```bash
   docker compose up -d && docker compose logs -f
    ```

## Funktionen

- Scraping von Polizeimeldungen von [Berlin.de](https://www.berlin.de/polizei/polizeimeldungen/)
- Speicherung von Meldungen in einer SQLite-Datenbank
- Bereitstellung der gespeicherten Daten als:
    - RSS-Feed
    - Atom-Feed
    - JSON-Format
- Automatisches Pruning von Einträgen, die älter als 6 Monate sind.

## TODOs

- [ ] Bereitstellung als Docker Image
- [ ] GitHub Actions für ^
- [ ] ToDo's aus dem Code angehen

## Lizenz

Dieses Projekt steht unter der [MIT-Lizenz](LICENSE).