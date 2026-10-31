package app

import (
	"github.com/pilef-co/bqto/internal/cli"

	_ "github.com/pilef-co/bqto/internal/alloydb"
	_ "github.com/pilef-co/bqto/internal/cloudsql"
	_ "github.com/pilef-co/bqto/internal/redis"
	_ "github.com/pilef-co/bqto/internal/valkey"
)

func Main(args []string) int {
	return cli.Main(args)
}
