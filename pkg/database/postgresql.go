// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package database

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

func init() {
	RegisteredDBInit["postgresql"] = NewPostgreSQLDatabase
	RegisteredDBCreators["postgresql"] = NewPostgreSQLCreator
	RegisteredDBStatements["postgresql"] = PostgreSQLStmts{}
}

type PostgreSQLCreator struct {
	env map[string]string
}

func NewPostgreSQLCreator(env map[string]string) DBCreator {
	return PostgreSQLCreator{env}
}

func (c PostgreSQLCreator) GetDriverName() string {
	return "postgres"
}

func (c PostgreSQLCreator) GetConnectionString() string {
	return fmt.Sprintf("user=%s password=%s dbname=%s host=%s port=%s sslmode=disable", c.env[DbUserEnvVar],
		c.env[DbPasswordEnvVar], c.env[DbNameEnvVar], c.env[DbHostEnvVar], c.env[DbPortEnvVar])
}

type PostgreSQLStmts struct{}

func (info PostgreSQLStmts) GetResourcesSQL() string {
	return "SELECT data FROM resource WHERE kind=$1 AND api_version=$2"
}

func (info PostgreSQLStmts) GetNamespacedResourcesSQL() string {
	return "SELECT data FROM resource WHERE kind=$1 AND api_version=$2 AND namespace=$3"
}

func (info PostgreSQLStmts) GetWriteResourceSQL() string {
	return "INSERT INTO resource (uuid, api_version, kind, name, namespace, resource_version, cluster_deleted_ts, data) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) " +
		"ON CONFLICT(uuid) DO UPDATE SET name=$4, namespace=$5, resource_version=$6, cluster_deleted_ts=$7, data=$8"
}

type PostgreSQLDatabase struct {
	*Database
}

func NewPostgreSQLDatabase(conn *sql.DB, stmts DBStatements) DBClient {
	return PostgreSQLDatabase{&Database{db: conn, stmts: stmts}}
}
