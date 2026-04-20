package main

import (
	"log"
	"os"

	_ "github.com/itsBOTzilla/dnsfoxv2-proto/gen/go/backups/v1"
	_ "github.com/itsBOTzilla/dnsfoxv2-proto/gen/go/billing/v1"
	_ "github.com/itsBOTzilla/dnsfoxv2-proto/gen/go/sites/v1"
	_ "github.com/itsBOTzilla/dnsfoxv2-proto/gen/go/warden/v1"
)

func main() {
	env := os.Getenv("ENVIRONMENT")
	if env == "" {
		env = "development"
	}
	log.Printf("dnsfox v2 workers starting — env %s", env)
	log.Println("workers: backup, billing, health, provisioning, scanner")
	log.Println("TODO: implement worker loops")
}
