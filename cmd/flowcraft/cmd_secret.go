package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/spf13/cobra"
)

const settingJWTSecret = "jwt_secret"

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretShowCmd, secretRotateCmd)
}

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secrets (JWT signing key)",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

var secretShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show JWT secret fingerprint (not the raw key)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()

		secret, err := s.GetSetting(ctx, settingJWTSecret)
		if err != nil {
			return fmt.Errorf("no JWT secret found; run 'flowcraft start' first to initialize")
		}

		hash := sha256.Sum256([]byte(secret))
		fingerprint := hex.EncodeToString(hash[:8])
		fmt.Printf("JWT secret fingerprint: %s\n", fingerprint)
		fmt.Printf("Secret length: %d bytes\n", len(secret))
		return nil
	},
}

var secretRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Rotate JWT secret (invalidates all existing tokens)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()

		newSecret := make([]byte, 32)
		if _, err := rand.Read(newSecret); err != nil {
			return fmt.Errorf("generate secret: %w", err)
		}
		encoded := hex.EncodeToString(newSecret)

		if err := s.SetSetting(ctx, settingJWTSecret, encoded); err != nil {
			return fmt.Errorf("save secret: %w", err)
		}

		hash := sha256.Sum256([]byte(encoded))
		fingerprint := hex.EncodeToString(hash[:8])
		fmt.Println("JWT secret rotated successfully.")
		fmt.Printf("New fingerprint: %s\n", fingerprint)
		fmt.Println("All existing JWT tokens are now invalid. Users must re-login.")
		return nil
	},
}

func openStore() (*store.SQLiteStore, error) {
	cfg := config.Load()
	dbPath := cfg.DBPath()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("database not found at %s; has the server been started?", dbPath)
	}
	return store.NewSQLiteStore(context.Background(), dbPath)
}
