package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/m4hi2/MeterAlertBot/internal/config"
	"github.com/m4hi2/MeterAlertBot/internal/datasources"
	"github.com/m4hi2/MeterAlertBot/internal/datasources/nesco"
	"github.com/muesli/coral"
)

var nescoCmd = &coral.Command{
	Use:   "nesco",
	Short: "NESCO utilities",
}

var balanceCmd = &coral.Command{
	Use:   "balance <account_number>",
	Short: "Fetch prepaid meter balance from NESCO",
	Args:  coral.ExactArgs(1),
	RunE: func(cmd *coral.Command, args []string) error {
		cfg := config.Get()
		svc := nesco.NewService(cfg.Nesco)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		bal, err := svc.GetBalance(ctx, datasources.Identifier{AccountNumber: args[0]})
		if err != nil {
			return fmt.Errorf("fetch failed: %w", err)
		}

		fmt.Printf("Account: %s\n", bal.AccountNumber)
		if bal.MeterNumber != "" {
			fmt.Printf("Meter:   %s\n", bal.MeterNumber)
		}
		fmt.Printf("Balance: %.2f BDT\n", bal.Balance)
		return nil
	},
}

func init() {
	nescoCmd.AddCommand(balanceCmd)
	rootCmd.AddCommand(nescoCmd)
}
