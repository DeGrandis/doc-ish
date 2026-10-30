# MyMedical

*(Working name.)* Every medical file your family has, in one place on your
own server, and a local AI that reads them and answers questions. A sibling
of [Proof of Basis](https://proofofbasis.degrand.is).

> **Not medical advice.** This is a tool for organizing your own records.
> It is not a doctor, and the assistant can be wrong. Talk to a clinician
> about anything that matters; in an emergency, call emergency services.

## What works today (milestone 1)

- **Set up once, encrypted at rest.** A password, plus a recovery key shown
  once. The database is encrypted page by page and every stored file is
  encrypted too; without the password or the recovery key nobody can read
  them. After a restart the records stay locked until someone signs in.
- **Profiles for the household.** One login, a person picker in the header.
  Each profile has the intake form a doctor's office asks for: born, sex,
  height, weight, blood type, allergies, medications, conditions, surgeries,
  family history, vaccinations, doctors, and more.
- **Drop anything.** Drag files onto the window (or use the paperclip, or
  the camera on a phone) and they're stored on the profile you're viewing.
  The same file twice is noticed.
- **Open files beside the chat.** PDFs, images, CSVs and text open next to
  the assistant.
- **Chat** with a model on your own Ollama, which sees the profile, the list
  of files and the latest measurements.

Reading the files themselves, putting lab values into a timeline, charts,
the DICOM viewer, reusable import recipes and the bundled medical reference
are the next milestones.

## Getting started

```sh
docker compose up -d --build
```

Open `http://<this computer's address>:8080`, set a password, and **write
down the recovery key**. Then in **Settings → Assistant**, check the Ollama
address and pick a model.

| Variable | Default | |
|---|---|---|
| `MYMEDICAL_ADDR` | `127.0.0.1:8080` (the image uses `0.0.0.0:8080`) | Listen address |
| `MYMEDICAL_DATA` | `./data` (the image uses `/data`) | Data directory |
| `MYMEDICAL_OLLAMA_URL` | unset | Fills in the Ollama address on a fresh install |
| `MYMEDICAL_PASSWORD` | unset | Unlocks the records whenever the app starts |

The name lives in one place, `internal/brand`, so it can be renamed later.

## Development

Go isn't required on the machine; the commands run in Docker:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine go test ./...
docker run --rm -p 127.0.0.1:8090:8080 -v "$PWD:/src" -w /src golang:1.26-alpine go run ./cmd/mymedical -addr 0.0.0.0:8080 -data /tmp/data
```

**Never commit real records.** `private/` and `*.real.*` are git-ignored.

### Layout

```
cmd/mymedical       the app
internal/brand      the product name
internal/vault      password and recovery key -> the data key
internal/store      SQLite, encrypted with the data key
internal/blobs      original files, encrypted with age
internal/ollama     the user's Ollama: models, streaming chat
internal/app        HTTP API and the assistant
web/static          UI (plain JavaScript, no build step)
```
