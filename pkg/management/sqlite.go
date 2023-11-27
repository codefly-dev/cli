package management

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"time"

	basev1 "github.com/codefly-dev/core/proto/v1/go/base"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/mattn/go-sqlite3"

	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"
)

type Sqlite struct {
	db           *sql.DB
	logBuffer    []*agentsv1.Log
	flushChannel chan bool
	clean        func(db *sql.DB)
	session      *basev1.Session
}

func (storage *Sqlite) AddLog(log *agentsv1.Log) {
	storage.logBuffer = append(storage.logBuffer, log)
	// Optionally, add logic here to handle buffer size limits
}

func (storage *Sqlite) flushLogsPeriodically() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			storage.flushLogs()
		case <-storage.flushChannel:
			return
		}
	}
}

func (storage *Sqlite) flushLogs() error {
	if len(storage.logBuffer) == 0 {
		return nil
	}
	err := insertLogs(storage.db, storage.session.Uuid, storage.logBuffer)
	if err != nil {
		return err
	}
	return nil
}

func (storage *Sqlite) Close() {
	storage.flushLogs()
	storage.clean(storage.db)
}

func (storage *Sqlite) StartSession(session *basev1.Session) error {
	storage.session = session
	err := storage.initSession(session)
	if err != nil {
		return err
	}
	go storage.flushLogsPeriodically()
	return nil
}

type Clean func()

func NewSqliteStorage() (Storage, error) {
	logger := shared.NewLogger("development.SqliteInit")
	ws, err := configurations.Current()
	if err != nil {
		return nil, err
	}
	dbFile := path.Join(ws.Dir(), "sessions.db")
	// Check if the file exists. If not, create it.
	if _, err := os.Stat(dbFile); os.IsNotExist(err) {
		file, err := os.Create(dbFile)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot create database file")
		}
		file.Close()
	}
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared&_fk=1", dbFile))
	if err != nil {
		return nil, logger.Wrapf(err, "cannot open database")
	}

	driver, err := sqlite.WithInstance(db, &sqlite.Config{})
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create driver")
	}

	d, err := iofs.New(migrations, "db")

	m, err := migrate.NewWithInstance(
		"iofs",
		d,
		"sqlite3",
		driver,
	)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create migration")
	}

	// Apply migrations
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return nil, logger.Wrapf(err, "Failed to apply migrations")
	}

	return &Sqlite{db: db, clean: func(db *sql.DB) { db.Close() }}, nil
}

func (storage *Sqlite) initSession(session *basev1.Session) error {
	switch session.Session.(type) {
	case *basev1.Session_Partial:
		return storage.initPartialSession(session)
	default:
		return errors.New("TBI")
	}
}

func (storage *Sqlite) initPartialSession(session *basev1.Session) error {
	logger := shared.NewLogger("development.initPartialSession")

	fmt.Println("PROJECT ID", session.GetPartial().Project.Uuid)
	err := storage.addProjectSnapshot(session.GetPartial().Project)
	if err != nil {
		return logger.Wrapf(err, "cannot add project snapshot")
	}
	fmt.Println("PARTIAL ID", session.GetPartial().Uuid)
	err = storage.addPartialSnapshot(session.GetPartial())
	if err != nil {
		return logger.Wrapf(err, "cannot add partial snapshot")
	}
	// SQL statement to insert a new session
	stmt, err := storage.db.Prepare(`INSERT INTO session (id, project_snapshot_id, partial_snapshot_id, application_snapshot_id, at) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return logger.Wrapf(err, "cannot prepare statement")
	}
	defer stmt.Close()

	// Execute the statement
	projectID := sql.NullString{String: session.GetPartial().Project.Uuid, Valid: true}
	partialID := sql.NullString{String: session.GetPartial().Uuid, Valid: true}
	applicationID := sql.NullString{String: "", Valid: false}
	_, err = stmt.Exec(session.Uuid, projectID, partialID, applicationID, session.At.AsTime())
	if err != nil {
		return logger.Wrapf(err, "cannot execute statement")
	}
	return nil
}

func (storage *Sqlite) addProjectSnapshot(project *basev1.ProjectSnapshot) error {
	logger := shared.NewLogger("development.SqliteAddProjectSnapshot")
	_, err := storage.db.Exec("INSERT INTO project_snapshot (id, name) VALUES (?, ?)", project.Uuid, project.Name)
	if err != nil {
		return logger.Wrapf(err, "cannot add project snapshot")
	}
	log.Println("ProjectSnapshot added successfully")
	return nil
}

func (storage *Sqlite) addPartialSnapshot(partial *basev1.PartialSnapshot) error {
	logger := shared.NewLogger("development.SqliteAddPartialSnapshot")

	stmt, err := storage.db.Prepare(`INSERT INTO partial_snapshot (id, name, project_id) VALUES (?, ?, ?)`)
	if err != nil {
		return logger.Wrapf(err, "cannot prepare statement")
	}
	defer stmt.Close()

	// Execute the statement
	_, err = stmt.Exec(partial.Uuid, partial.Name, partial.Project.Uuid)
	if err != nil {
		return logger.Wrapf(err, "cannot execute statement")
	}
	log.Println("PartialSnapshot added successfully")
	return nil
}

func insertLogs(db *sql.DB, sessionID string, logs []*agentsv1.Log) error {
	// Start a transaction
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// Prepare the insert statement
	stmt, err := tx.Prepare("INSERT INTO logs (session_id, at, application, service, kind, message) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	// Insert each log in the batch
	for _, log := range logs {
		_, err = stmt.Exec(sessionID, log.At.AsTime(), log.Application, log.Service, log.Kind, log.Message)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	// Commit the transaction
	return tx.Commit()
}

//go:embed db
var migrations embed.FS
