package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Recipe is an import recipe: a Starlark script for one file format.
type Recipe struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Description string          `json:"description"`
	Fingerprint string          `json:"fingerprint"`
	Script      string          `json:"script"`
	Status      string          `json:"status"` // proposed, approved, rejected
	DocumentID  *int64          `json:"document_id,omitempty"`
	Preview     json.RawMessage `json:"preview,omitempty"`
	Created     string          `json:"created"`
	Approved    string          `json:"approved,omitempty"`
	Uses        int             `json:"uses"`
	LastUsed    string          `json:"last_used,omitempty"`
}

// Recipe statuses.
const (
	RecipeProposed = "proposed"
	RecipeApproved = "approved"
	RecipeRejected = "rejected"
)

const recipeCols = `id, name, kind, description, fingerprint, script, status, document_id, preview, created, approved, uses, last_used`

func scanRecipe(row interface{ Scan(...any) error }) (Recipe, error) {
	var r Recipe
	var preview string
	err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Description, &r.Fingerprint, &r.Script, &r.Status, &r.DocumentID, &preview,
		&r.Created, &r.Approved, &r.Uses, &r.LastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if preview != "" {
		r.Preview = json.RawMessage(preview)
	}
	return r, err
}

// ProposeRecipe saves a draft for a file, setting aside any earlier draft
// for the same file.
func (s *Store) ProposeRecipe(r Recipe) (Recipe, error) {
	var id int64
	err := s.Tx(func(tx *sql.Tx) error {
		if r.DocumentID != nil {
			if _, err := tx.Exec(`UPDATE recipes SET status = 'rejected' WHERE document_id = ? AND status = 'proposed'`, *r.DocumentID); err != nil {
				return err
			}
		}
		res, err := tx.Exec(`INSERT INTO recipes(name, kind, description, fingerprint, script, status, document_id, preview, created)
			VALUES(?, ?, ?, ?, ?, 'proposed', ?, ?, ?)`, r.Name, r.Kind, r.Description, r.Fingerprint, r.Script, r.DocumentID, string(r.Preview), now())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return r, err
	}
	return s.Recipe(id)
}

func (s *Store) Recipe(id int64) (Recipe, error) {
	return scanRecipe(s.db.QueryRow(`SELECT `+recipeCols+` FROM recipes WHERE id = ?`, id))
}

// RecipeTx reads a recipe inside a transaction.
func RecipeTx(tx *sql.Tx, id int64) (Recipe, error) {
	return scanRecipe(tx.QueryRow(`SELECT `+recipeCols+` FROM recipes WHERE id = ?`, id))
}

// ApprovedRecipe is the approved recipe for a format, if there is one
// (the most recently approved, if several).
func (s *Store) ApprovedRecipe(fingerprint string) (Recipe, error) {
	return scanRecipe(s.db.QueryRow(`SELECT `+recipeCols+` FROM recipes WHERE fingerprint = ? AND status = 'approved'
		ORDER BY approved DESC, id DESC LIMIT 1`, fingerprint))
}

// ProposedRecipe is the draft waiting for approval for a file, if any.
func (s *Store) ProposedRecipe(docID int64) (Recipe, error) {
	return scanRecipe(s.db.QueryRow(`SELECT `+recipeCols+` FROM recipes WHERE document_id = ? AND status = 'proposed'
		ORDER BY id DESC LIMIT 1`, docID))
}

// Recipes lists the approved recipes, most used first.
func (s *Store) Recipes() ([]Recipe, error) {
	rows, err := s.db.Query(`SELECT ` + recipeCols + ` FROM recipes WHERE status = 'approved' ORDER BY uses DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recipe{}
	for rows.Next() {
		r, err := scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRecipeStatusTx approves, rejects or reopens a recipe.
func SetRecipeStatusTx(tx *sql.Tx, id int64, status string) error {
	approved := ""
	if status == RecipeApproved {
		approved = now()
	}
	_, err := tx.Exec(`UPDATE recipes SET status = ?, approved = ? WHERE id = ?`, status, approved, id)
	return err
}

func (s *Store) SetRecipeStatus(id int64, status string) error {
	return s.Tx(func(tx *sql.Tx) error { return SetRecipeStatusTx(tx, id, status) })
}

// UsedRecipeTx counts a use.
func UsedRecipeTx(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`UPDATE recipes SET uses = uses + 1, last_used = ? WHERE id = ?`, now(), id)
	return err
}

func (s *Store) DeleteRecipe(id int64) error {
	_, err := s.db.Exec(`DELETE FROM recipes WHERE id = ?`, id)
	return err
}

// SetDocumentRecipeTx records which recipe read a file (0 for none).
func SetDocumentRecipeTx(tx *sql.Tx, docID, recipeID int64) error {
	_, err := tx.Exec(`UPDATE documents SET recipe_id = ? WHERE id = ?`, nullID(recipeID), docID)
	return err
}
