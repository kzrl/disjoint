package main

import (
	"database/sql"
	"fmt"
	"os"

	"disjoint/db"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func main() {
	dbPath := os.Args[1]
	dsn := fmt.Sprintf("%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	goose.SetBaseFS(db.MigrationsFS)
	if err := goose.SetDialect("sqlite"); err != nil {
		panic(err)
	}
	if err := goose.Up(conn, "migrations"); err != nil {
		panic(err)
	}
	fmt.Println("migrations applied")
}
