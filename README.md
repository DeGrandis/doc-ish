# MyMedical

*(Working name.)* Every medical file your family has, in one place on your
own server, and a local AI that reads them and answers questions. A sibling
of [Proof of Basis](https://proofofbasis.degrand.is).

> **Not medical advice.** This is a tool for organizing your own records.
> It is not a doctor, and the assistant can be wrong. Talk to a clinician
> about anything that matters; in an emergency, call emergency services.

## What works today (milestones 1 to 6)

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
- **Chat is the app.** The assistant, on your own Ollama, sees the
  profile, each file's summary and the latest value of everything
  measured, and has tools for the rest:
  - "Weight 180.5 this morning" saves the value; "I take vitamin D 2000 IU"
    adds it to the profile. Each change shows with an **Undo** button.
  - "How is my weight trending?" draws a chart in the chat, with the
    reference range as a band and out-of-range readings in red.
  - "Pull up my metabolic panel" opens it beside the chat, and "this" then
    means that file.
  - It can search files, read one in full, and look up every reading of
    anything over time.
  - "Help me fill in my profile" (or **Fill in with the assistant** on the
    Profile page) starts an intake interview that saves as it goes.

- **Import recipes for long tables.** A device export with hundreds or
  thousands of readings (a glucose meter, a blood pressure cuff, a scale)
  is too long for the model to copy value by value. Instead the model
  writes a small program for that format, in
  [Starlark](https://github.com/bazelbuild/starlark) (a small dialect of
  Python made for running code you don't fully trust). The app tries it on
  the file, shows the model any error, and keeps the draft that works.
  Review shows what it found across the whole file, the first and last
  values, and the script itself. Approve it and every later file in that
  format is read by the recipe instantly, without the model. If it's
  wrong, say what's wrong and it writes another.
  - Recipes run with nothing but the file's text and a few helpers (CSV,
    numbers with decimal commas, dates in many forms, JSON). They have no
    files, network or clock, a cap on steps and values, and a time limit.
  - A draft that skips most of a table's rows is sent back to the model
    rather than accepted, and any shortfall left is shown in Review.
  - Settings lists the approved recipes, how often each was used, and their
    scripts.

- **A medical reference on your server.** The assistant looks things up
  instead of trusting what a small model remembers, and says where it
  read it:
  - **MedlinePlus** health topics (National Library of Medicine): plain
    explanations of about a thousand conditions, tests and terms. Built in,
    with an Update button.
  - **RxNorm** (NLM): every prescribable drug's ingredients, brands and
    strengths. One button, about 70 MB.
  - **FDA drug labels** (openFDA): what each drug is for, its warnings,
    and its drug interactions section, so "can I take ibuprofen with my
    lisinopril?" is answered from the labels. One button; about 1.8 GB to
    download, about 100 MB kept.
  - **LOINC** lab test names: its license needs a free loinc.org account,
    so you download the release zip yourself and drop it in Settings.

  The app contacts these sources only when you press a button in
  Settings, and nothing about what you look up is sent anywhere.

- **Imaging (X-ray, CT, MRI, ultrasound).** Drop DICOM files, a whole
  folder from a hospital CD, or a zip of one: images are gathered into
  studies by their own ids, series by series, and the same image twice is
  noticed. A report PDF or text on the CD becomes a file of its own;
  the CD's index and viewer program are left out. A study is described
  from its tags (no model needed) and held in Review if the name in it
  isn't the profile's.
  - **The viewer** shows each series as a thumbnail, then its images slice
    by slice (scroll, drag or the slider), with brightness and contrast
    (window and level) done in the browser from the full 16-bit values,
    CT presets (soft tissue, lung, bone, brain, liver), invert, zoom and
    pan, on a phone too. Download gives the whole study as a zip.
  - Uncompressed and JPEG images are read directly; JPEG Lossless,
    JPEG-LS, JPEG 2000 and RLE, common on CDs, go through GDCM, which the
    Docker image includes.
  - **The report** is linked to its study automatically (an imaging report
    within three days of it), or by hand from the viewer.
  - **"Ask about this image"**: the vision model looks at the main
    series' middle image together with the linked report, and the
    assistant says what it notices and what to ask, as an AI's look at one
    image, with the radiologist's report as what counts.

Backups, the model benchmark for this app and release builds are the last
milestone.

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

`dev.Dockerfile` is a Go image with the same tools as the app image
(Poppler, GDCM), for running tests and the dev server. Invented imaging
studies for trying the viewer:

```sh
docker run --rm -v "$PWD:/src" -w /src mymedical-devbase go run ./cmd/mymedical imaging synth -out ./_dev/synth
```

The MedlinePlus topics built into the binary are refreshed with:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine go run ./cmd/mymedical reference bundle-medlineplus
```

### Credits

Health topics: MedlinePlus, National Library of Medicine. Drug names:
RxNorm, National Library of Medicine. Drug labels: data provided by the
U.S. Food and Drug Administration (open.fda.gov). LOINC, when you add it:
this material contains content from LOINC (http://loinc.org), copyright ©
Regenstrief Institute, Inc. and the LOINC Committee, available at no cost
under the license at http://loinc.org/license.

### Layout

```
cmd/mymedical       the app
internal/brand      the product name
internal/vault      password and recovery key -> the data key
internal/store      SQLite, encrypted with the data key
internal/blobs      original files, encrypted with age
internal/ollama     the user's Ollama: models, streaming chat, structured answers
internal/ingest     reading files: extraction, the reader's prompt, the checks for Review
internal/recipes    import recipes: the Starlark sandbox, table fingerprints, drafting with the model
internal/reference  MedlinePlus (bundled), RxNorm, FDA labels and LOINC, and the lookups over them
internal/imaging    DICOM: tags, pixels (with GDCM for compressed images), rendering, invented test studies
internal/openai     an opt-in hosted provider in the OpenAI format
internal/app        HTTP API, the background reader, Review, Activity, the assistant
web/static          UI (plain JavaScript, no build step)
```
