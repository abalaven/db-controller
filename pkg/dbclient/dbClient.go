package dbclient

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/lib/pq"

	"github.com/infobloxopen/db-controller/pkg/metrics"
)

const (
	PostgresType = "postgres"
)

var extensions = []string{"citext", "uuid-ossp", "pgcrypto"}

type DBClient interface {
	CreateDataBase(dbName string) (bool, error)
	CreateUser(username, role, userPassword string) (bool, error)
	CreateGroup(dbName, username string) (bool, error)
	RenameUser(oldUsername string, newUsername string) error
	UpdateUser(oldUsername, newUsername, rolename, password string) error
	UpdatePassword(username string, userPassword string) error

	DBCloser
}

type DBCloser interface {
	Close() error
}

type PostgresClient struct {
	dbType string
	dbURL  string
	DB     *sql.DB
	log    logr.Logger
}

func (p *PostgresClient) getDB(dbname string) (*sql.DB, error) {
	u, err := url.Parse(p.dbURL)
	if err != nil {
		return nil, err
	}
	u.Path = "/" + dbname
	return sql.Open("postgres", u.String())
}

func DBClientFactory(log logr.Logger, dbType, host, port, user, password, sslmode string) (DBClient, error) {
	switch dbType {
	case PostgresType:
		return NewPostgresClient(log, dbType, host, port, user, password, sslmode)
	default:
		return NewPostgresClient(log, dbType, host, port, user, password, sslmode)
	}
}

// creates postgres client
func NewPostgresClient(log logr.Logger, dbType, host, port, user, password, sslmode string) (*PostgresClient, error) {
	db, err := sql.Open(PostgresType, PostgresConnectionString(host, port, user, password, "postgres", sslmode))
	if err != nil {
		return nil, err
	}
	return &PostgresClient{
		dbType: dbType,
		DB:     db,
		log:    log,
		dbURL:  PostgresURI(host, port, user, password, "", sslmode),
	}, nil
}

func PostgresConnectionString(host, port, user, password, dbname, sslmode string) string {
	return fmt.Sprintf("host='%s' port='%s' user='%s' password='%s' dbname='%s' sslmode='%s'", host,
		port, escapeValue(user), escapeValue(password), escapeValue(dbname), sslmode)
}

func PostgresURI(host, port, user, password, dbname, sslmode string) string {
	connURL := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     fmt.Sprintf("%s:%s", host, port),
		Path:     fmt.Sprintf("/%s", dbname),
		RawQuery: fmt.Sprintf("sslmode=%s", sslmode),
	}

	return connURL.String()
}

func (pc *PostgresClient) CreateDataBase(dbName string) (bool, error) {
	var exists bool
	created := false
	db := pc.DB
	err := db.QueryRow("SELECT EXISTS(SELECT datname FROM pg_catalog.pg_database WHERE datname = $1)", dbName).Scan(&exists)

	if err != nil {
		pc.log.Error(err, "could not query for database name")
		metrics.DBProvisioningErrors.WithLabelValues("read error")
		return created, err
	}
	if !exists {
		pc.log.Info("creating DB:", "database name", dbName)
		// create the database
		if _, err := db.Exec(fmt.Sprintf("create database %s", pq.QuoteIdentifier(dbName))); err != nil {
			pc.log.Error(err, "could not create database")
			metrics.DBProvisioningErrors.WithLabelValues("create error")
			return created, err
		}
		created = true
		pc.log.Info("database has been created", "DB", dbName)
		metrics.DBCreated.Inc()
	}
	// db is now database specific connection
	db, err = pc.getDB(dbName)
	pc.log.Info("connected to " + dbName)
	if err != nil {
		pc.log.Error(err, "could not connect to db", "database", dbName)
		return created, err
	}
	defer db.Close()
	for _, s := range extensions {
		if _, err = db.Exec(fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", pq.QuoteIdentifier(s))); err != nil {
			pc.log.Error(err, "could not create extension", "database_name", dbName)
			return created, fmt.Errorf("could not create extension %s: %s", s, err)
		}
		pc.log.Info("created extension " + s)
	}
	return created, err
}

func (pc *PostgresClient) CreateGroup(dbName, rolename string) (bool, error) {
	start := time.Now()
	var exists bool
	db := pc.DB
	created := false

	err := db.QueryRow("SELECT EXISTS(SELECT pg_roles.rolname FROM pg_catalog.pg_roles where pg_roles.rolname = $1)", rolename).Scan(&exists)
	if err != nil {
		pc.log.Error(err, "could not query for role")
		metrics.UsersCreatedErrors.WithLabelValues("read error").Inc()
		return created, err
	}

	if !exists {
		pc.log.Info("creating a ROLE", "role", rolename)
		_, err = pc.DB.Exec(fmt.Sprintf("CREATE ROLE %s WITH NOLOGIN", pq.QuoteIdentifier(rolename)))
		if err != nil {
			pc.log.Error(err, "could not create role "+rolename)
			metrics.UsersCreatedErrors.WithLabelValues("create error").Inc()
			return created, err
		}

		if _, err := db.Exec(fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s", pq.QuoteIdentifier(dbName), pq.QuoteIdentifier(rolename))); err != nil {
			pc.log.Error(err, "could not set permissions to role "+rolename)
			metrics.UsersCreatedErrors.WithLabelValues("grant error").Inc()
			return created, err
		}
		created = true
		pc.log.Info("role has been created", "role", rolename)
		metrics.UsersCreated.Inc()
		duration := time.Since(start)
		metrics.UsersCreateTime.Observe(duration.Seconds())
	}

	return created, nil
}

func (pc *PostgresClient) setGroup(username, rolename string) error {
	db := pc.DB
	if _, err := db.Exec(fmt.Sprintf("ALTER ROLE %s SET ROLE TO %s", pq.QuoteIdentifier(username), pq.QuoteIdentifier(rolename))); err != nil {
		return err
	}

	return nil
}

func (pc *PostgresClient) CreateUser(username, rolename, userPassword string) (bool, error) {
	start := time.Now()
	var exists bool
	db := pc.DB
	created := false

	err := db.QueryRow("SELECT EXISTS(SELECT pg_user.usename FROM pg_catalog.pg_user where pg_user.usename = $1)", username).Scan(&exists)
	if err != nil {
		pc.log.Error(err, "could not query for user name")
		metrics.UsersCreatedErrors.WithLabelValues("read error").Inc()
		return created, err
	}

	if !exists {
		pc.log.Info("creating a user", "user", username)

		s := fmt.Sprintf("CREATE ROLE %s with encrypted password %s LOGIN IN ROLE %s", pq.QuoteIdentifier(username), pq.QuoteLiteral(userPassword), pq.QuoteIdentifier(rolename))
		_, err = pc.DB.Exec(s)
		if err != nil {
			pc.log.Error(err, "could not create user "+username)
			metrics.UsersCreatedErrors.WithLabelValues("create error").Inc()
			return created, err
		}

		if err := pc.setGroup(username, rolename); err != nil {
			pc.log.Error(err, fmt.Sprintf("could not set role %s to user %s", rolename, username))
			metrics.UsersCreatedErrors.WithLabelValues("grant error").Inc()

			return created, err
		}

		created = true
		pc.log.Info("user has been created", "user", username)
		metrics.UsersCreated.Inc()
		duration := time.Since(start)
		metrics.UsersCreateTime.Observe(duration.Seconds())
	}

	return created, nil
}

func (pc *PostgresClient) RenameUser(oldUsername string, newUsername string) error {
	var exists bool
	db := pc.DB

	err := db.QueryRow("SELECT EXISTS(SELECT pg_roles.rolname FROM pg_catalog.pg_roles where pg_roles.rolname = $1)", oldUsername).Scan(&exists)

	if err != nil {
		pc.log.Error(err, "could not query for user name")
		return err
	}

	if exists {
		pc.log.Info(fmt.Sprintf("renaming user %v to %v", oldUsername, newUsername))

		_, err = db.Exec(fmt.Sprintf("ALTER USER %s RENAME TO %s", pq.QuoteIdentifier(oldUsername), pq.QuoteIdentifier(newUsername)))
		if err != nil {
			pc.log.Error(err, "could not rename user "+oldUsername)
			return err
		}
	}

	return nil
}

func (pc *PostgresClient) UpdateUser(oldUsername, newUsername, rolename, password string) error {
	start := time.Now()
	var exists bool
	db := pc.DB

	err := db.QueryRow("SELECT EXISTS(SELECT pg_roles.rolname FROM pg_catalog.pg_roles where pg_roles.rolname = $1)", oldUsername).Scan(&exists)
	if err != nil {
		pc.log.Error(err, "could not query for user name")
		metrics.UsersUpdatedErrors.WithLabelValues("read error").Inc()
		return err
	}

	if exists {
		pc.log.Info(fmt.Sprintf("updating user %s", oldUsername))
		if err := pc.RenameUser(oldUsername, newUsername); err != nil {
			return err
		}

		if err := pc.setGroup(newUsername, rolename); err != nil {
			pc.log.Error(err, fmt.Sprintf("could not set role %s to user %s", rolename, newUsername))
			metrics.UsersCreatedErrors.WithLabelValues("grant error").Inc()

			return err
		}

		if err := pc.UpdatePassword(newUsername, password); err != nil {
			return err
		}

		pc.log.Info("user has been updated", "user", newUsername)
		metrics.UsersUpdated.Inc()
		duration := time.Since(start)
		metrics.UsersUpdateTime.Observe(duration.Seconds())
	}

	return nil
}

func (pc *PostgresClient) UpdatePassword(username string, userPassword string) error {
	start := time.Now()
	db := pc.DB
	if userPassword == "" {
		err := fmt.Errorf("an empty password")
		pc.log.Error(err, "error occurred")
		metrics.PasswordRotatedErrors.WithLabelValues("empty password").Inc()
		return err
	}

	pc.log.Info("update user password", "user:", username)
	_, err := db.Exec(fmt.Sprintf("ALTER ROLE %s with encrypted password %s", pq.QuoteIdentifier(username), pq.QuoteLiteral(userPassword)))
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			pc.log.Error(err, "could not alter user "+username)
			metrics.PasswordRotatedErrors.WithLabelValues("alter error").Inc()
			return err
		}
	}
	metrics.PasswordRotated.Inc()
	duration := time.Since(start)
	metrics.PasswordRotateTime.Observe(duration.Seconds())

	return nil
}

func (pc *PostgresClient) Close() error {
	if pc.DB != nil {
		return pc.DB.Close()
	}

	return fmt.Errorf("can't close nil DB")
}

func escapeValue(in string) string {

	encoded := make([]rune, 0)
	for _, c := range in {
		switch c {
		case ' ':
			encoded = append(encoded, '\\')
			encoded = append(encoded, ' ')
		case '\\':
			encoded = append(encoded, '\\')
			encoded = append(encoded, '\\')
		case '\'':
			encoded = append(encoded, '\\')
			encoded = append(encoded, '\'')
		default:
			encoded = append(encoded, c)
		}
	}
	return string(encoded)
}
