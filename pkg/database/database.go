// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kubearchive/kubearchive/pkg/models"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type newDatabaseFunc func(*sql.DB, DBStatements) DBClient
type newCreatorFunc func(map[string]string) DBCreator

var RegisteredDBInit = make(map[string]newDatabaseFunc)
var RegisteredDBCreators = make(map[string]newCreatorFunc)
var RegisteredDBStatements = make(map[string]DBStatements)

// DBCreator provides the needed information to establish a connection to the database
type DBCreator interface {
	GetDriverName() string
	GetConnectionString() string
}

// DBStatements provides the needed information to run the SQL statements by the database client
type DBStatements interface {
	GetResourcesSQL() string
	GetNamespacedResourcesSQL() string
	GetWriteResourceSQL() string
}

// DBClient provides the logic to interact with the DB
type DBClient interface {
	QueryResources(ctx context.Context, kind, group, version string) ([]*unstructured.Unstructured, error)
	QueryCoreResources(ctx context.Context, kind, version string) ([]*unstructured.Unstructured, error)
	QueryNamespacedResources(ctx context.Context, kind, group, version, namespace string) ([]*unstructured.Unstructured, error)
	QueryNamespacedCoreResources(ctx context.Context, kind, version, namespace string) ([]*unstructured.Unstructured, error)
	WriteResource(ctx context.Context, k8sObj *unstructured.Unstructured, data []byte) error
	Ping(ctx context.Context) error
}

type Database struct {
	db    *sql.DB
	stmts DBStatements
}

func NewDatabase() (DBClient, error) {
	// Extract the environment vars
	env, err := newDatabaseEnvironment()
	if err != nil {
		return nil, err
	}
	// Get the type of DB
	dbKind := env[DbKindEnvVar]

	// Get the registered creator, statements and client for the type of DB
	var creator DBCreator
	if c, ok := RegisteredDBCreators[dbKind]; ok {
		creator = c(env)
	} else {
		panic(fmt.Sprintf("No database registered with name %s", dbKind))
	}

	var stmts DBStatements
	if s, ok := RegisteredDBStatements[dbKind]; ok {
		stmts = s
	} else {
		panic(fmt.Sprintf("No database registered with name %s", dbKind))
	}

	// Get DB Connection
	conn := establishConnection(creator.GetDriverName(), creator.GetConnectionString())

	var database DBClient
	if init, ok := RegisteredDBInit[dbKind]; ok {
		database = init(conn, stmts)
	} else {
		panic(fmt.Sprintf("No database registered with name %s", dbKind))
	}

	// Instantiate the DB Client
	return database, nil
}

func (db *Database) Ping(ctx context.Context) error {
	return db.db.PingContext(ctx)
}

func (db *Database) QueryResources(ctx context.Context, kind, group, version string) ([]*unstructured.Unstructured, error) {
	apiVersion := fmt.Sprintf("%s/%s", group, version)
	return db.performResourceQuery(ctx, db.stmts.GetResourcesSQL(), kind, apiVersion)
}

func (db *Database) QueryCoreResources(ctx context.Context, kind, version string) ([]*unstructured.Unstructured, error) {
	return db.performResourceQuery(ctx, db.stmts.GetResourcesSQL(), kind, version)
}

func (db *Database) QueryNamespacedResources(ctx context.Context, kind, group, version, namespace string) ([]*unstructured.Unstructured, error) {
	apiVersion := fmt.Sprintf("%s/%s", group, version)
	return db.performResourceQuery(ctx, db.stmts.GetNamespacedResourcesSQL(), kind, apiVersion, namespace)
}

func (db *Database) QueryNamespacedCoreResources(ctx context.Context, kind, version, namespace string) ([]*unstructured.Unstructured, error) {
	return db.performResourceQuery(ctx, db.stmts.GetNamespacedResourcesSQL(), kind, version, namespace)
}

func (db *Database) performResourceQuery(ctx context.Context, query string, args ...string) ([]*unstructured.Unstructured, error) {
	castedArgs := make([]interface{}, len(args))
	for i, v := range args {
		castedArgs[i] = v
	}
	rows, err := db.db.QueryContext(ctx, query, castedArgs...)
	if err != nil {
		return nil, err
	}
	defer func(rows *sql.Rows) {
		err = rows.Close()
	}(rows)
	var resources []*unstructured.Unstructured
	if err != nil {
		return resources, err
	}
	for rows.Next() {
		var b sql.RawBytes
		var r *unstructured.Unstructured
		if err := rows.Scan(&b); err != nil {
			return resources, err
		}
		if r, err = models.UnstructuredFromByteSlice([]byte(b)); err != nil {
			return resources, err
		}
		resources = append(resources, r)
	}
	return resources, err
}

func (db *Database) WriteResource(ctx context.Context, k8sObj *unstructured.Unstructured, data []byte) error {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not begin transaction for resource %s: %s", k8sObj.GetUID(), err)
	}
	_, execErr := tx.ExecContext(
		ctx,
		db.stmts.GetWriteResourceSQL(),
		k8sObj.GetUID(),
		k8sObj.GetAPIVersion(),
		k8sObj.GetKind(),
		k8sObj.GetName(),
		k8sObj.GetNamespace(),
		k8sObj.GetResourceVersion(),
		models.OptionalTimestamp(k8sObj.GetDeletionTimestamp()),
		data,
	)
	if execErr != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return fmt.Errorf("write to database failed: %s and unable to roll back transaction: %s", execErr, rollbackErr)
		}
		return fmt.Errorf("write to database failed: %s", execErr)
	}
	execErr = tx.Commit()
	if execErr != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return fmt.Errorf("commit to database failed: %s and unable to roll back transaction: %s", execErr, rollbackErr)
		}
		return fmt.Errorf("commit to database failed and the transactions was rolled back: %s", execErr)
	}
	return nil
}
