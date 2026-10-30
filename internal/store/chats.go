package store

// Message is one turn of a chat.
type Message struct {
	ID      int64  `json:"id"`
	ChatID  int64  `json:"chat_id"`
	Role    string `json:"role"` // user, assistant
	Content string `json:"content"`
	Created string `json:"created"`
}

// Chat is a conversation on one profile.
type Chat struct {
	ID       int64  `json:"id"`
	PersonID int64  `json:"person_id"`
	Title    string `json:"title"`
	Created  string `json:"created"`
	Updated  string `json:"updated"`
}

func (s *Store) NewChat(personID int64, title string) (int64, error) {
	t := now()
	res, err := s.db.Exec(`INSERT INTO chats(person_id, title, created, updated) VALUES(?, ?, ?, ?)`, personID, title, t, t)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ChatOwner(chatID int64) (int64, error) {
	var pid int64
	err := s.db.QueryRow(`SELECT person_id FROM chats WHERE id = ?`, chatID).Scan(&pid)
	if err != nil {
		return 0, ErrNotFound
	}
	return pid, nil
}

func (s *Store) Chats(personID int64) ([]Chat, error) {
	rows, err := s.db.Query(`SELECT id, person_id, title, created, updated FROM chats WHERE person_id = ? ORDER BY updated DESC LIMIT 50`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.PersonID, &c.Title, &c.Created, &c.Updated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddMessage(chatID int64, role, content string) error {
	t := now()
	if _, err := s.db.Exec(`INSERT INTO messages(chat_id, role, content, created) VALUES(?, ?, ?, ?)`, chatID, role, content, t); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE chats SET updated = ? WHERE id = ?`, t, chatID)
	return err
}

func (s *Store) Messages(chatID int64) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id, chat_id, role, content, created FROM messages WHERE chat_id = ? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &m.Created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteChat(id int64) error {
	_, err := s.db.Exec(`DELETE FROM chats WHERE id = ?`, id)
	return err
}
