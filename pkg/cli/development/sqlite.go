package development

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log"
	"os"
	"path"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/mattn/go-sqlite3"
)

type ProjectReference struct {
	ID string
	*corev1.ProjectReference
}

type Session struct {
	ID string
	*corev1.Session
}

type Sqlite struct {
	db *sql.DB
}

func (s *Sqlite) Init(session *corev1.Session) error {

	switch x := session.Session.(type) {
	case *corev1.Session_Partial:
		return addPartialReference(s.db, x.Partial)
	default:
		return errors.New("TBI")
	}
}

func NewSqliteStorage() (Storage, error) {
	db, err := SqliteInit()
	if err != nil {
		return nil, err
	}
	return &Sqlite{db: db}, nil
}

func SqliteInit() (*sql.DB, error) {
	ws, err := configurations.Current()
	if err != nil {
		return nil, err
	}
	dbFile := path.Join(ws.Dir(), "sessions.db")
	// Check if the file exists. If not, create it.
	if _, err := os.Stat(dbFile); os.IsNotExist(err) {
		file, err := os.Create(dbFile)
		if err != nil {
			log.Fatalf("Failed to create the database file: %s", err)
		}
		file.Close()
	}
	logger := shared.NewLogger("development.SqliteInit")
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared&_fk=1", dbFile))
	if err != nil {
		return nil, logger.Wrapf(err, "cannot open database")
	}

	defer func(db *sql.DB) {
		err := db.Close()
		if err != nil {
			logger.Warn(shared.NewUserWarning("cannot close database"))
		}
	}(db)

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
	return db, nil
}

func startSession(db *sql.DB, session *corev1.Session) (*Session, error) {
	// SQL statement to insert a new session
	stmt, err := db.Prepare(`INSERT INTO session (id, at) VALUES (?, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	// Execute the statement
	res, err := stmt.Exec(uuid.New(), session.At.AsTime())
	if err != nil {
		return nil, err
	}

	log.Println("Session added successfully")
	return nil
}

func getOrCreateProject(db *sql.DB, projectName string) (*ProjectReference, error) {
	// Check if the project exists
	var project ProjectReference
	err := db.QueryRow("SELECT id, name FROM project_reference WHERE name = ?", projectName).Scan(&project.ID, &project.Name)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Project does not exist, create it
			_, err := db.Exec("INSERT INTO project_reference (id, name) VALUES (?, ?)", uuid.New(), projectName)
			if err != nil {
				return nil, err
			}

			return &project, nil
		} else {
			// Some other error occurred
			return nil, err
		}
	}

	// Project exists, return it
	return &project, nil
}

func addPartialReference(db *sql.DB, partial *corev1.PartialReference) error {
	// SQL statement to insert a new partial reference
	project, err := getOrCreateProject(db, partial.Project.Name)
	if err != nil {

	}
	stmt, err := db.Prepare(`INSERT INTO partial_reference (id, name, project_id) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// Execute the statement
	_, err = stmt.Exec(project.ID, partial.Name, project.ID)
	if err != nil {
		return err
	}

	log.Println("PartialReference added successfully")
	return nil
}

//go:embed db
var migrations embed.FS
