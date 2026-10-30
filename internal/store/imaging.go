package store

import (
	"database/sql"
	"errors"
)

// An imaging study (one visit's scan) is one document, made of series
// (one sequence or view each), made of instances (one image file each,
// sometimes with several frames). Hospital CDs hold hundreds of files for
// one study; they all land on the same document.

const imagingSchema = `
CREATE TABLE IF NOT EXISTS studies (
  id           INTEGER PRIMARY KEY,
  document_id  INTEGER NOT NULL UNIQUE REFERENCES documents(id) ON DELETE CASCADE,
  person_id    INTEGER NOT NULL,
  study_uid    TEXT NOT NULL,
  study_date   TEXT NOT NULL DEFAULT '',
  modality     TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  body_part    TEXT NOT NULL DEFAULT '',
  institution  TEXT NOT NULL DEFAULT '',
  patient_name TEXT NOT NULL DEFAULT '',
  report_id    INTEGER REFERENCES documents(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS studies_uid ON studies(person_id, study_uid);
CREATE TABLE IF NOT EXISTS series (
  id          INTEGER PRIMARY KEY,
  study_id    INTEGER NOT NULL REFERENCES studies(id) ON DELETE CASCADE,
  series_uid  TEXT NOT NULL,
  number      INTEGER NOT NULL DEFAULT 0,
  description TEXT NOT NULL DEFAULT '',
  modality    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS series_uid ON series(study_id, series_uid);
CREATE TABLE IF NOT EXISTS instances (
  id        INTEGER PRIMARY KEY,
  series_id INTEGER NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  sop_uid   TEXT NOT NULL,
  number    INTEGER NOT NULL DEFAULT 0,
  position  REAL NOT NULL DEFAULT 0,
  frames    INTEGER NOT NULL DEFAULT 1,
  rows      INTEGER NOT NULL DEFAULT 0,
  cols      INTEGER NOT NULL DEFAULT 0,
  blob      TEXT NOT NULL,
  size      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS instances_uid ON instances(series_id, sop_uid);
`

// Study is one imaging study.
type Study struct {
	ID          int64    `json:"id"`
	DocumentID  int64    `json:"document_id"`
	PersonID    int64    `json:"person_id"`
	StudyUID    string   `json:"study_uid"`
	Date        string   `json:"date"`
	Modality    string   `json:"modality"`
	Description string   `json:"description"`
	BodyPart    string   `json:"body_part"`
	Institution string   `json:"institution"`
	PatientName string   `json:"patient_name"`
	ReportID    *int64   `json:"report_id,omitempty"`
	Series      []Series `json:"series,omitempty"`
}

// Series is one sequence or view in a study, with its images in order.
type Series struct {
	ID          int64      `json:"id"`
	Number      int        `json:"number"`
	Description string     `json:"description"`
	Modality    string     `json:"modality"`
	Instances   []Instance `json:"instances"`
}

// Instance is one image file.
type Instance struct {
	ID       int64   `json:"id"`
	SeriesID int64   `json:"-"`
	SOPUID   string  `json:"-"`
	Number   int     `json:"number"`
	Position float64 `json:"position"`
	Frames   int     `json:"frames"`
	Rows     int     `json:"rows"`
	Cols     int     `json:"cols"`
	Blob     string  `json:"-"`
	Size     int64   `json:"-"`
}

const studyCols = `id, document_id, person_id, study_uid, study_date, modality, description, body_part, institution, patient_name, report_id`

func scanStudy(row interface{ Scan(...any) error }) (Study, error) {
	var s Study
	err := row.Scan(&s.ID, &s.DocumentID, &s.PersonID, &s.StudyUID, &s.Date, &s.Modality, &s.Description, &s.BodyPart,
		&s.Institution, &s.PatientName, &s.ReportID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// StudyByUID finds a profile's study by its DICOM id.
func StudyByUIDTx(tx *sql.Tx, personID int64, uid string) (Study, error) {
	return scanStudy(tx.QueryRow(`SELECT `+studyCols+` FROM studies WHERE person_id = ? AND study_uid = ?`, personID, uid))
}

// AddStudyTx records a new study on its document.
func AddStudyTx(tx *sql.Tx, s Study) (int64, error) {
	res, err := tx.Exec(`INSERT INTO studies(document_id, person_id, study_uid, study_date, modality, description, body_part, institution, patient_name)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, s.DocumentID, s.PersonID, s.StudyUID, s.Date, s.Modality, s.Description, s.BodyPart, s.Institution, s.PatientName)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SeriesIDTx finds or makes a series in a study.
func SeriesIDTx(tx *sql.Tx, studyID int64, uid string, number int, description, modality string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM series WHERE study_id = ? AND series_uid = ?`, studyID, uid).Scan(&id)
	if err == nil {
		return id, nil
	}
	res, err := tx.Exec(`INSERT INTO series(study_id, series_uid, number, description, modality) VALUES(?, ?, ?, ?, ?)`,
		studyID, uid, number, description, modality)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddInstanceTx records an image file, or reports false when the series
// already has it (the same image dropped twice).
func AddInstanceTx(tx *sql.Tx, in Instance) (bool, error) {
	var n int
	tx.QueryRow(`SELECT count(*) FROM instances WHERE series_id = ? AND sop_uid = ?`, in.SeriesID, in.SOPUID).Scan(&n)
	if n > 0 {
		return false, nil
	}
	_, err := tx.Exec(`INSERT INTO instances(series_id, sop_uid, number, position, frames, rows, cols, blob, size) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.SeriesID, in.SOPUID, in.Number, in.Position, in.Frames, in.Rows, in.Cols, in.Blob, in.Size)
	return err == nil, err
}

// StudyCountsTx is how many series and images a study has, and their size.
func StudyCountsTx(tx *sql.Tx, studyID int64) (series, images int, size int64, err error) {
	err = tx.QueryRow(`SELECT count(DISTINCT s.id), count(i.id), coalesce(sum(i.size), 0) FROM series s LEFT JOIN instances i ON i.series_id = s.id
		WHERE s.study_id = ?`, studyID).Scan(&series, &images, &size)
	return
}

// StudyOf finds the study a document holds, with its series and images in
// viewing order.
func (s *Store) StudyOf(docID int64) (Study, error) {
	st, err := scanStudy(s.db.QueryRow(`SELECT `+studyCols+` FROM studies WHERE document_id = ?`, docID))
	if err != nil {
		return st, err
	}
	rows, err := s.db.Query(`SELECT id, number, description, modality FROM series WHERE study_id = ? ORDER BY number, id`, st.ID)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var sr Series
		rows.Scan(&sr.ID, &sr.Number, &sr.Description, &sr.Modality)
		st.Series = append(st.Series, sr)
	}
	rows.Close()
	for i := range st.Series {
		ir, err := s.db.Query(`SELECT id, number, position, frames, rows, cols FROM instances WHERE series_id = ? ORDER BY number, position, id`, st.Series[i].ID)
		if err != nil {
			return st, err
		}
		st.Series[i].Instances = []Instance{}
		for ir.Next() {
			var in Instance
			ir.Scan(&in.ID, &in.Number, &in.Position, &in.Frames, &in.Rows, &in.Cols)
			st.Series[i].Instances = append(st.Series[i].Instances, in)
		}
		ir.Close()
	}
	return st, nil
}

// StudyByID reads a study row.
func (s *Store) StudyByID(id int64) (Study, error) {
	return scanStudy(s.db.QueryRow(`SELECT `+studyCols+` FROM studies WHERE id = ?`, id))
}

// Studies lists a profile's studies, for matching reports.
func (s *Store) Studies(personID int64) ([]Study, error) {
	rows, err := s.db.Query(`SELECT `+studyCols+` FROM studies WHERE person_id = ? ORDER BY study_date`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Study
	for rows.Next() {
		st, err := scanStudy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Instance reads one image file's record, with the profile it belongs to.
func (s *Store) Instance(id int64) (Instance, int64, error) {
	var in Instance
	var pid int64
	err := s.db.QueryRow(`SELECT i.id, i.series_id, i.sop_uid, i.number, i.position, i.frames, i.rows, i.cols, i.blob, i.size, st.person_id
		FROM instances i JOIN series s ON s.id = i.series_id JOIN studies st ON st.id = s.study_id WHERE i.id = ?`, id).
		Scan(&in.ID, &in.SeriesID, &in.SOPUID, &in.Number, &in.Position, &in.Frames, &in.Rows, &in.Cols, &in.Blob, &in.Size, &pid)
	if errors.Is(err, sql.ErrNoRows) {
		return in, 0, ErrNotFound
	}
	return in, pid, err
}

// StudyBlobs lists every image file of the study a document holds, for
// deleting them with it or downloading them.
func (s *Store) StudyBlobs(docID int64) ([]Instance, error) {
	rows, err := s.db.Query(`SELECT i.id, i.sop_uid, i.blob, i.size, i.number, s.number FROM instances i JOIN series s ON s.id = i.series_id
		JOIN studies st ON st.id = s.study_id WHERE st.document_id = ? ORDER BY s.number, i.number, i.id`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var in Instance
		var seriesNum int
		rows.Scan(&in.ID, &in.SOPUID, &in.Blob, &in.Size, &in.Number, &seriesNum)
		in.SeriesID = int64(seriesNum)
		out = append(out, in)
	}
	return out, rows.Err()
}

// SetStudyReportTx links a study to its report (0 to unlink).
func SetStudyReportTx(tx *sql.Tx, studyID, reportID int64) error {
	_, err := tx.Exec(`UPDATE studies SET report_id = ? WHERE id = ?`, nullID(reportID), studyID)
	return err
}

// MoveStudyTx gives a study to the profile its document moved to.
func MoveStudyTx(tx *sql.Tx, docID, personID int64) error {
	_, err := tx.Exec(`UPDATE studies SET person_id = ? WHERE document_id = ?`, personID, docID)
	return err
}
