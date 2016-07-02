package main

import (
	"os"

	log "github.com/Sirupsen/logrus"
)

// getEnvReq retrieves the value of the environment variable named by the key.
// It returns the value, or logs a fatal message if the variable does not exist.
func getEnvReq(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.WithField("key", key).Fatal("Missing required env var")
	}
	return val
}

// getEnvDef retrieves the value of the environment variable named by the key.
// It returns the value, or returns the specified default if the variable does not exist.
func getEnvDef(key string, def string) string {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	return val
}
