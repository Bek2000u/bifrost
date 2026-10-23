package configstore

import (
	"context"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

func migrationAddBillingReceipts(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return RunSingleMigration(ctx, nil, db, logger, &migrator.Migration{
		ID: "add_billing_receipts",
		Migrate: func(tx *gorm.DB) error {
			if err := tx.WithContext(ctx).AutoMigrate(&tables.TableBillingGateway{}, &tables.TableBillingExecution{}, &tables.TableBillingEvent{}); err != nil {
				return err
			}
			return addColumnIfNotExists(tx, logger, &tables.TableProviderJob{}, "BillingExecutionID")
		},
	})
}
