package main

import (
	"log"
	"os"
	"strconv"

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

	WoTThreshold float64

	WorkingDirectory string
	RelayPort        string
	PrivateKey       string
	DefaultLimit     int
}

func LoadConfig() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v\n", err)
	}

	dl, err := strconv.Atoi(os.Getenv("DEFAULT_LIMIT"))
	if err != nil {
		log.Fatalf("Error reading DEFAULT_LIMIT: %v\n", err)
	}

	wt, err := strconv.ParseFloat(os.Getenv("WOT_THRESHOLD"), 64)
	if err != nil {
		log.Fatalf("Error reading WOT_THRESHOLD: %v\n", err)
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
		PrivateKey:       os.Getenv("PRIVATE_KEY"),
		WoTThreshold:     wt,
		DefaultLimit:     dl,
	}
}
