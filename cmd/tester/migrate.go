package main

import (
	"context"
	"log"

	"github.com/digitalocean/tester/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "apply pending database migrations and exit",
	Long: `migrate applies the database migrations that "tester serve" would
otherwise run at startup, then exits 0. It is meant to run once per deploy,
before the server and runners start (an App Platform PRE_DEPLOY job), so that
the server can be started with --migrate-on-start=false.

The DSN is taken from --pg-dsn, MIGRATE_PG_DSN, or, failing both, SERVE_PG_DSN
so the job can reuse the web service's environment as is.`,
	Args: cobra.ExactArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		dsn := viper.GetString("migrate-pg-dsn")
		if dsn == "" {
			dsn = viper.GetString("serve-pg-dsn")
		}
		if dsn == "" {
			log.Fatal("no postgresql dsn: set --pg-dsn, MIGRATE_PG_DSN or SERVE_PG_DSN")
		}

		ctx := context.Background()
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			log.Fatalf("failed to configure db pool: %s", err)
		}
		defer pool.Close()
		err = pool.Ping(ctx)
		if err != nil {
			log.Fatalf("failed to connect to db: %s", err)
		}

		dbStore := db.NewPG(pool)
		log.Print("applying database migrations")
		err = dbStore.Init(ctx)
		if err != nil {
			log.Fatalf("failed to migrate db: %s", err)
		}
		log.Print("database schema is current")
	},
}

func init() {
	migrateCmd.Flags().String("pg-dsn", "", "The postgresql dsn to use.")
	viper.BindPFlag("migrate-pg-dsn", migrateCmd.Flags().Lookup("pg-dsn"))
}
