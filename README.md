# Doc-ish

Every medical file your family has, in one place on your
own server, and a local AI that reads them and answers questions. A sibling
of [Proof of Basis](https://proofofbasis.degrand.is).

> **Not medical advice.** This is a tool for organizing your own records.
> It is not a doctor, and the assistant can be wrong. Talk to a clinician
> about anything that matters; in an emergency, call emergency services.

## What works today

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
- **Long files are read a few pages at a time**: a 100-page records
  release, a binder of scanned faxes. The header and footer printed on
  every page are taken off (and shown once); each part is told the last
  section heading and date, and the header of a table that runs over from
  the page before, since tables only print it above their first rows.
  Text is laid out at a fixed pitch so a column sits in the same place on
  every page, and tables are rewritten cell by cell (`| Ferritin |  | 18 L
  | 41 |`), blank cells kept, with the header printed again on each page a
  table runs onto: on an invented 24-page release with a lab table of
  three dates over four pages, a 4B model went from 25 to 78 of its 80
  values with their right dates. Every value keeps its own date, the same
  value read twice is kept once, and the file is named and summed up from
  all its parts, dated by its newest result.
- **The records inside a long file** are found as sections of their own:
  each part of the file says which record every page belongs to (a visit
  note, a lab report, a letter, a handout, with its date), and pages of
  one record are joined. The file stays one file, but its viewer lists
  what's inside by date and page, picking one shows that record's values
  and turns the PDF to its page, and on the timeline each visit is on its
  own day and a cumulative lab table once for each day it has results
  for. Values and follow-ups say which record they came from, so a
  "recheck in six months" counts from that visit's date. On the invented
  release, qwen3.5:4b found all three visits and the lab table on the
  right pages and dates.
- **Scans and photos of paperwork** are read by Tesseract first (in the
  image, no model, about a second a page on the CPU). It notes where on
  the page every word is, so a scanned page is laid out like a printed
  one, columns in place and blank cells blank, and read the same way:
  on the release above as a 130 dpi scan, the 4B model got 78 of 80
  values with their right dates, as on the printed copy. A page it can't
  read with confidence (a phone photo, handwriting, a picture) goes to
  an optional OCR model (Settings → Assistant → Model for reading scans;
  `deepseek-ocr` on Ollama, about 6 GB, fits a 12 GB card), or else to
  the vision model. An OCR model writes tables out as rows of cells and
  can leave a blank cell out, which slides the values after it into the
  wrong column; a file where that may have happened waits in Review.
- **A progress bar** under the menu shows an upload going up, its
  encryption, and then the reading: which pages of a long file, how far
  along, and how many more files wait.
- **Follow-ups.** The reader picks out what a file says should happen
  later ("Recheck blood count and iron studies in about six months",
  "Follow-up CT in 12 months") and works out the due date from the
  note's own date. They're listed as **Coming up** on the home page and
  the timeline, overdue ones marked, with Done and Not needed buttons;
  you or the assistant can add your own ("remind me about the dentist in
  three months"). A later value, scan, visit or vaccination that shows
  one done closes it, saying what showed it (a recheck of iron studies
  closes when iron or ferritin results dated after it arrive), and undoing
  that file opens it again.
- **Timeline**: a person's history on one line, newest first: every
  file by the date it's about (with its out-of-range values), the dates in
  the profile (a medication started, a diagnosis, a surgery, a vaccine),
  and follow-ups done, by year, with filters for visits, labs, imaging,
  medications and vaccines.
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

- **Phone and watch exports.** Drop an iPhone's Apple Health `export.zip`
  (or just its `export.xml`), or a Google Takeout zip with Google Fit in it,
  and the app imports it with its own code, no model, streaming it so a
  several-gigabyte export is fine.
  - Weight, blood pressure, glucose, temperature, resting heart rate and
    the like are kept reading by reading (or as a daily average when a
    sensor records them many times a day). Heart rate, steps, distance,
    energy, exercise minutes and sleep become one value a day. Steps
    counted by both a phone and a watch are counted once, not twice.
  - Hospital records in Apple's export (if the person connected their
    providers) bring in lab results with their ranges and flags, and add
    allergies, active conditions and medications, and vaccinations to the
    profile without overwriting anything entered by hand. Apple Watch ECGs
    come in with their classification.
  - Exports hold the whole history every time, so next month's adds only
    what's new. A birth date or a name in it that isn't the profile's
    holds it in Review. One Undo takes an import back out, profile
    additions included.
  - The assistant gets averages (overall and month by month) with every
    lookup, so "what was my average blood pressure in September?" is
    quoted, not worked out by a small model.
- **Backups.** Settings → Backups downloads everything as one `.tar`
  (it asks for your password again first), or writes one to a folder on a
  schedule, keeping the newest few: point it at a NAS share mounted into
  the container. A backup stays encrypted and opens only with the password
  or recovery key the records had when it was made. To move to a new
  server, choose **Restore from a backup** on its setup screen. Restoring
  is only possible on a fresh install, so it can never overwrite records.
- **A benchmark** for choosing a model: the real app, invented records,
  rule-graded questions. See [docs/benchmark.md](docs/benchmark.md).

## Getting started

```sh
docker compose up -d --build
```

Open `http://<this computer's address>:8080`, set a password, and **write
down the recovery key**. Then in **Settings → Assistant**, check the Ollama
address and pick a model.

| Variable | Default | |
|---|---|---|
| `DOCISH_ADDR` | `127.0.0.1:8080` (the image uses `0.0.0.0:8080`) | Listen address |
| `DOCISH_DATA` | `./data` (the image uses `/data`) | Data directory |
| `DOCISH_OLLAMA_URL` | unset | Fills in the Ollama address on a fresh install |
| `DOCISH_PASSWORD` | unset | Unlocks the records whenever the app starts |
| `DOCISH_BACKUP_DIR` | unset | The folder for automatic backups, until Settings changes it |

### Images and CI

`.github/workflows/images.yml` runs on every push to `main`: gofmt, vet and
tests (with Poppler and GDCM installed, so the PDF and compressed-DICOM
tests run), then builds the image for `linux/amd64` and `linux/arm64` and
publishes it to GitHub's container registry as `ghcr.io/<owner>/<repo>`,
tagged with the commit and `:latest`. A version tag (`git tag v1.2.3`)
also publishes `:1.2.3` with the version stamped into the app. Pull
requests run the same steps without publishing.

The name lives in one place, `internal/brand`, so it can be renamed later.

## Development

Go isn't required on the machine; the commands run in Docker:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine go test ./...
docker run --rm -p 127.0.0.1:8090:8080 -v "$PWD:/src" -w /src golang:1.26-alpine go run ./cmd/docish -addr 0.0.0.0:8080 -data /tmp/data
```

**Never commit real records.** `private/` and `*.real.*` are git-ignored.

`dev.Dockerfile` is a Go image with the same tools as the app image
(Poppler, GDCM), for running tests and the dev server. Invented imaging
studies for trying the viewer:

```sh
docker run --rm -v "$PWD:/src" -w /src docish-devbase go run ./cmd/docish imaging synth -out ./_dev/synth
```

The MedlinePlus topics built into the binary are refreshed with:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26-alpine go run ./cmd/docish reference bundle-medlineplus
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
cmd/docish       the app
internal/brand      the product name
internal/vault      password and recovery key -> the data key
internal/store      SQLite, encrypted with the data key
internal/blobs      original files, encrypted with age
internal/ollama     the user's Ollama: models, streaming chat, structured answers
internal/ingest     reading files: extraction, the reader's prompt, the checks for Review
internal/recipes    import recipes: the Starlark sandbox, table fingerprints, drafting with the model
internal/reference  MedlinePlus (bundled), RxNorm, FDA labels and LOINC, and the lookups over them
internal/imaging    DICOM: tags, pixels (with GDCM for compressed images), rendering, invented test studies
internal/healthexport  Apple Health and Google Fit exports, and hospital records in FHIR; invented test exports
internal/pdfgen     simple text PDFs, and an invented 24-page records release, for tests
internal/openai     an opt-in hosted provider in the OpenAI format
internal/bench      the model benchmark
internal/app        HTTP API, the background reader, Review, Activity, the assistant
web/static          UI (plain JavaScript, no build step)
```
