// Command migrate applies embedded goose migrations (MySQL dialect).
// Usage: migrate [up|down|status|version]  (default: up)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/example/wechat/internal/platform/config"
	"github.com/example/wechat/migrations"
	"github.com/example/wechat/internal/platform/mysqlx"
)

func main() {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	db, err := mysqlx.Open(cfg.MySQL.DSN, cfg.MySQL.MaxOpenConns, cfg.MySQL.MaxIdleConns, cfg.MySQL.ConnMaxLifetime)
	if err != nil {
		log.Fatalf("mysql: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("mysql"); err != nil {
		log.Fatalf("goose dialect: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch cmd {
	case "up":
		err = goose.UpContext(ctx, db, ".")
	case "down":
		err = goose.DownContext(ctx, db, ".")
	case "status":
		err = goose.StatusContext(ctx, db, ".")
	case "version":
		var v int64
		v, err = goose.GetDBVersionContext(ctx, db)
		if err == nil {
			fmt.Println("version:", v)
		}
	default:
		log.Fatalf("unknown command %q (up|down|status|version)", cmd)
	}
	if err != nil {
		log.Fatalf("migrate %s: %v", cmd, err)
	}
}
