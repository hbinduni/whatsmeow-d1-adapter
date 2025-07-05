package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/hbinduni/whatsmeow-d1-adapter/adapter/d1"
)

func main() {
	// Set up logging
	logger := waLog.Stdout("Main", "DEBUG", true)

	// Get D1 configuration from environment variables
	apiToken := os.Getenv("D1_API_TOKEN")
	accountID := os.Getenv("D1_ACCOUNT_ID")
	databaseID := os.Getenv("D1_DATABASE_ID")

	// Validate configuration
	if accountID == "" || databaseID == "" || apiToken == "" {
		log.Fatal("D1_ACCOUNT_ID, D1_DATABASE_ID, and D1_API_TOKEN are all required")
	}

	// Create D1 container
	container, err := d1.NewContainer(d1.D1ContainerConfig{
		APIToken:   apiToken,
		AccountID:  accountID,
		DatabaseID: databaseID,
		Logger:     logger,
	})
	if err != nil {
		log.Fatalf("Failed to create D1 container: %v", err)
	}
	defer container.Close()

	// Get or create a device
	deviceStore := container.NewDevice()

	// Create WhatsApp client
	client := whatsmeow.NewClient(deviceStore, logger)

	// Add event handler
	client.AddEventHandler(func(evt any) {
		logger.Infof("Received event: %T", evt)
	})

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, new login
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Printf("QR code: %s\n", evt.Code)
			} else {
				logger.Infof("Login event: %s", evt.Event)
			}
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
	}

	logger.Infof("WhatsApp client connected successfully!")
	logger.Infof("Device JID: %s", client.Store.ID)

	// Wait for interrupt signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Infof("Shutting down...")
	client.Disconnect()
}
