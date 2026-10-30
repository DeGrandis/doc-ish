# MyMedical

*(Working name.)* Every medical file your family has, in one place on your
own server, and a local AI that reads them and answers questions. A sibling
of [Proof of Basis](https://proofofbasis.degrand.is).

> **Not medical advice.** This is a tool for organizing your own records.
> It is not a doctor, and the assistant can be wrong. Talk to a clinician
> about anything that matters; in an emergency, call emergency services.

## What works today (milestones 1 and 2)

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
- **It reads what you drop.** In the background, one file at a time, a
  model on your Ollama works out what each file is (lab results, imaging,
  a visit note, a prescription…), its date, provider and whose it is, and
  copies out every result with its unit, range and flag. Text PDFs go
  through Poppler; scans and photos go to a vision model; DICOM files are
  described from their own tags; Word files, CSVs, XML and text are read
  directly.
- **Review** holds anything it isn't sure about: a name that doesn't match
  the profile (with a one-click move to the right person), a date that
  can't be right, a big table it only sampled, or a part it couldn't read.
  Nothing from those files is filed until you say so.
- **Activity** lists every change, by you or the reader, and the latest
  change to anything can be undone.
- **Open files beside the chat**, with what was read from them: the
  summary, every value (out-of-range ones flagged), and medications named.
- **Chat** with a model on your own Ollama, which sees the profile, each
  file's summary and the latest value of every measurement.

Charts, opening files from the chat, the DICOM viewer, reusable import
recipes for big tables, and the bundled medical reference are the next
milestones.

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
internal/ollama     the user's Ollama: models, streaming chat, structured answers
internal/ingest     reading files: extraction, the reader's prompt, the checks for Review
internal/app        HTTP API, the background reader, Review, Activity, the assistant
web/static          UI (plain JavaScript, no build step)
```
