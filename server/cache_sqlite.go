package server

import (
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3" // SQLite driver
	"log"
	"strings"
	"time"
)

// Messages cache
const (
	createMessagesTableQuery = `
		BEGIN;
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY,
			time INT NOT NULL,
			topic TEXT NOT NULL,
			message TEXT NOT NULL,
			title TEXT NOT NULL,
			priority INT NOT NULL,
			tags TEXT NOT NULL,
			click TEXT NOT NULL,
			attachment_name TEXT NOT NULL,
			attachment_type TEXT NOT NULL,
			attachment_size INT NOT NULL,
			attachment_expires INT NOT NULL,
			attachment_url TEXT NOT NULL,
			attachment_owner TEXT NOT NULL,
			encoding TEXT NOT NULL,
			published INT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_topic ON messages (topic);
		COMMIT;
	`
	insertMessageQuery = `
		INSERT INTO messages (id, time, topic, message, title, priority, tags, click, attachment_name, attachment_type, attachment_size, attachment_expires, attachment_url, attachment_owner, encoding, published) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	pruneMessagesQuery           = `DELETE FROM messages WHERE time < ? AND published = 1`
	selectMessagesSinceTimeQuery = `
		SELECT id, time, topic, message, title, priority, tags, click, attachment_name, attachment_type, attachment_size, attachment_expires, attachment_url, attachment_owner, encoding
		FROM messages 
		WHERE topic = ? AND time >= ? AND published = 1
		ORDER BY time ASC
	`
	selectMessagesSinceTimeIncludeScheduledQuery = `
		SELECT id, time, topic, message, title, priority, tags, click, attachment_name, attachment_type, attachment_size, attachment_expires, attachment_url, attachment_owner, encoding
		FROM messages 
		WHERE topic = ? AND time >= ?
		ORDER BY time ASC
	`
	selectMessagesDueQuery = `
		SELECT id, time, topic, message, title, priority, tags, click, attachment_name, attachment_type, attachment_size, attachment_expires, attachment_url, attachment_owner, encoding
		FROM messages 
		WHERE time <= ? AND published = 0
	`
	updateMessagePublishedQuery     = `UPDATE messages SET published = 1 WHERE id = ?`
	selectMessagesCountQuery        = `SELECT COUNT(*) FROM messages`
	selectMessageCountForTopicQuery = `SELECT COUNT(*) FROM messages WHERE topic = ?`
	selectTopicsQuery               = `SELECT topic FROM messages GROUP BY topic`
	selectAttachmentsSizeQuery      = `SELECT IFNULL(SUM(attachment_size), 0) FROM messages WHERE attachment_owner = ? AND attachment_expires >= ?`
	selectAttachmentsExpiredQuery   = `SELECT id FROM messages WHERE attachment_expires > 0 AND attachment_expires < ?`
)

// Schema management queries
const (
	currentSchemaVersion          = 4
	createSchemaVersionTableQuery = `
		CREATE TABLE IF NOT EXISTS schemaVersion (
			id INT PRIMARY KEY,
			version INT NOT NULL
		);
	`
	insertSchemaVersion      = `INSERT INTO schemaVersion VALUES (1, ?)`
	updateSchemaVersion      = `UPDATE schemaVersion SET version = ? WHERE id = 1`
	selectSchemaVersionQuery = `SELECT version FROM schemaVersion WHERE id = 1`

	// 0 -> 1
	migrate0To1AlterMessagesTableQuery = `
		BEGIN;
		ALTER TABLE messages ADD COLUMN title TEXT NOT NULL DEFAULT('');
		ALTER TABLE messages ADD COLUMN priority INT NOT NULL DEFAULT(0);
		ALTER TABLE messages ADD COLUMN tags TEXT NOT NULL DEFAULT('');
		COMMIT;
	`

	// 1 -> 2
	migrate1To2AlterMessagesTableQuery = `
		ALTER TABLE messages ADD COLUMN published INT NOT NULL DEFAULT(1);
	`

	// 2 -> 3
	migrate2To3AlterMessagesTableQuery = `
		BEGIN;
		ALTER TABLE messages ADD COLUMN click TEXT NOT NULL DEFAULT('');
		ALTER TABLE messages ADD COLUMN attachment_name TEXT NOT NULL DEFAULT('');
		ALTER TABLE messages ADD COLUMN attachment_type TEXT NOT NULL DEFAULT('');
		ALTER TABLE messages ADD COLUMN attachment_size INT NOT NULL DEFAULT('0');
		ALTER TABLE messages ADD COLUMN attachment_expires INT NOT NULL DEFAULT('0');
		ALTER TABLE messages ADD COLUMN attachment_owner TEXT NOT NULL DEFAULT('');
		ALTER TABLE messages ADD COLUMN attachment_url TEXT NOT NULL DEFAULT('');
		COMMIT;
	`
	// 3 -> 4
	migrate3To4AlterMessagesTableQuery = `
		ALTER TABLE messages ADD COLUMN encoding TEXT NOT NULL DEFAULT('');
	`
)

type sqliteCache struct {
	db *sql.DB
}

var _ cache = (*sqliteCache)(nil)

func newSqliteCache(filename string) (*sqliteCache, error) {
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return nil, err
	}
	if err := setupDB(db); err != nil {
		return nil, err
	}
	return &sqliteCache{
		db: db,
	}, nil
}

func (c *sqliteCache) AddMessage(m *message) error {
	if m.Event != messageEvent {
		return errUnexpectedMessageType
	}
	published := m.Time <= time.Now().Unix()
	tags := strings.Join(m.Tags, ",")
	var attachmentName, attachmentType, attachmentURL, attachmentOwner string
	var attachmentSize, attachmentExpires int64
	if m.Attachment != nil {
		attachmentName = m.Attachment.Name
		attachmentType = m.Attachment.Type
		attachmentSize = m.Attachment.Size
		attachmentExpires = m.Attachment.Expires
		attachmentURL = m.Attachment.URL
		attachmentOwner = m.Attachment.Owner
	}
	_, err := c.db.Exec(
		insertMessageQuery,
		m.ID,
		m.Time,
		m.Topic,
		m.Message,
		m.Title,
		m.Priority,
		tags,
		m.Click,
		attachmentName,
		attachmentType,
		attachmentSize,
		attachmentExpires,
		attachmentURL,
		attachmentOwner,
		m.Encoding,
		published,
	)
	return err
}

func (c *sqliteCache) Messages(topic string, since sinceTime, scheduled bool) ([]*message, error) {
	if since.IsNone() {
		return make([]*message, 0), nil
	}
	var rows *sql.Rows
	var err error
	if scheduled {
		rows, err = c.db.Query(selectMessagesSinceTimeIncludeScheduledQuery, topic, since.Time().Unix())
	} else {
		rows, err = c.db.Query(selectMessagesSinceTimeQuery, topic, since.Time().Unix())
	}
	if err != nil {
		return nil, err
	}
	return readMessages(rows)
}

func (c *sqliteCache) MessagesDue() ([]*message, error) {
	rows, err := c.db.Query(selectMessagesDueQuery, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return readMessages(rows)
}

func (c *sqliteCache) MarkPublished(m *message) error {
	_, err := c.db.Exec(updateMessagePublishedQuery, m.ID)
	return err
}

func (c *sqliteCache) MessageCount(topic string) (int, error) {
	rows, err := c.db.Query(selectMessageCountForTopicQuery, topic)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int
	if !rows.Next() {
		return 0, errors.New("no rows found")
	}
	if err := rows.Scan(&count); err != nil {
		return 0, err
	} else if err := rows.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func (c *sqliteCache) Topics() (map[string]*topic, error) {
	rows, err := c.db.Query(selectTopicsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	topics := make(map[string]*topic)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		topics[id] = newTopic(id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return topics, nil
}

func (c *sqliteCache) Prune(olderThan time.Time) error {
	_, err := c.db.Exec(pruneMessagesQuery, olderThan.Unix())
	return err
}

func (c *sqliteCache) AttachmentsSize(owner string) (int64, error) {
	rows, err := c.db.Query(selectAttachmentsSizeQuery, owner, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var size int64
	if !rows.Next() {
		return 0, errors.New("no rows found")
	}
	if err := rows.Scan(&size); err != nil {
		return 0, err
	} else if err := rows.Err(); err != nil {
		return 0, err
	}
	return size, nil
}

func (c *sqliteCache) AttachmentsExpired() ([]string, error) {
	rows, err := c.db.Query(selectAttachmentsExpiredQuery, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func readMessages(rows *sql.Rows) ([]*message, error) {
	defer rows.Close()
	messages := make([]*message, 0)
	for rows.Next() {
		var timestamp, attachmentSize, attachmentExpires int64
		var priority int
		var id, topic, msg, title, tagsStr, click, attachmentName, attachmentType, attachmentURL, attachmentOwner, encoding string
		err := rows.Scan(
			&id,
			&timestamp,
			&topic,
			&msg,
			&title,
			&priority,
			&tagsStr,
			&click,
			&attachmentName,
			&attachmentType,
			&attachmentSize,
			&attachmentExpires,
			&attachmentURL,
			&attachmentOwner,
			&encoding,
		)
		if err != nil {
			return nil, err
		}
		var tags []string
		if tagsStr != "" {
			tags = strings.Split(tagsStr, ",")
		}
		var att *attachment
		if attachmentName != "" && attachmentURL != "" {
			att = &attachment{
				Name:    attachmentName,
				Type:    attachmentType,
				Size:    attachmentSize,
				Expires: attachmentExpires,
				URL:     attachmentURL,
				Owner:   attachmentOwner,
			}
		}
		messages = append(messages, &message{
			ID:         id,
			Time:       timestamp,
			Event:      messageEvent,
			Topic:      topic,
			Message:    msg,
			Title:      title,
			Priority:   priority,
			Tags:       tags,
			Click:      click,
			Attachment: att,
			Encoding:   encoding,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func setupDB(db *sql.DB) error {
	// If 'messages' table does not exist, this must be a new database
	rowsMC, err := db.Query(selectMessagesCountQuery)
	if err != nil {
		return setupNewDB(db)
	}
	rowsMC.Close()

	// If 'messages' table exists, check 'schemaVersion' table
	schemaVersion := 0
	rowsSV, err := db.Query(selectSchemaVersionQuery)
	if err == nil {
		defer rowsSV.Close()
		if !rowsSV.Next() {
			return errors.New("cannot determine schema version: cache file may be corrupt")
		}
		if err := rowsSV.Scan(&schemaVersion); err != nil {
			return err
		}
		rowsSV.Close()
	}

	// Do migrations
	if schemaVersion == currentSchemaVersion {
		return nil
	} else if schemaVersion == 0 {
		return migrateFrom0(db)
	} else if schemaVersion == 1 {
		return migrateFrom1(db)
	} else if schemaVersion == 2 {
		return migrateFrom2(db)
	} else if schemaVersion == 3 {
		return migrateFrom3(db)
	}
	return fmt.Errorf("unexpected schema version found: %d", schemaVersion)
}

func setupNewDB(db *sql.DB) error {
	if _, err := db.Exec(createMessagesTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(createSchemaVersionTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(insertSchemaVersion, currentSchemaVersion); err != nil {
		return err
	}
	return nil
}

func migrateFrom0(db *sql.DB) error {
	log.Print("Migrating cache database schema: from 0 to 1")
	if _, err := db.Exec(migrate0To1AlterMessagesTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(createSchemaVersionTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(insertSchemaVersion, 1); err != nil {
		return err
	}
	return migrateFrom1(db)
}

func migrateFrom1(db *sql.DB) error {
	log.Print("Migrating cache database schema: from 1 to 2")
	if _, err := db.Exec(migrate1To2AlterMessagesTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(updateSchemaVersion, 2); err != nil {
		return err
	}
	return migrateFrom2(db)
}

func migrateFrom2(db *sql.DB) error {
	log.Print("Migrating cache database schema: from 2 to 3")
	if _, err := db.Exec(migrate2To3AlterMessagesTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(updateSchemaVersion, 3); err != nil {
		return err
	}
	return migrateFrom3(db)
}

func migrateFrom3(db *sql.DB) error {
	log.Print("Migrating cache database schema: from 3 to 4")
	if _, err := db.Exec(migrate3To4AlterMessagesTableQuery); err != nil {
		return err
	}
	if _, err := db.Exec(updateSchemaVersion, 4); err != nil {
		return err
	}
	return nil // Update this when a new version is added
}
