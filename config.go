package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	RelayName        string
	RelayPubkey      string
	RelayDescription string
	RelayURL         string
	RelayContact     string
	RelayIcon        string
	RelayBanner      string

	WorkingDirectory string

	RelayPort string
}

func LoadConfig() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	config = Config{
		RelayName:        os.Getenv("RELAY_NAME"),
		RelayPubkey:      os.Getenv("RELAY_PUBKEY"),
		RelayDescription: os.Getenv("RELAY_DESCRIPTION"),
		RelayURL:         os.Getenv("RELAY_URL"),
		RelayContact:     os.Getenv("RELAY_CONTACT"),
		RelayIcon:        os.Getenv("RELAY_ICON"),
		RelayBanner:      os.Getenv("RELAY_BANNER"),
		WorkingDirectory: os.Getenv("WORKING_DIR"),
		RelayPort:        os.Getenv("RELAY_PORT"),
	}
}
