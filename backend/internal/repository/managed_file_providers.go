package repository

import (
	"database/sql"
	"github.com/MACOS-DO/sub4api/internal/service"
)

func ProvideManagedFileRepository(db *sql.DB) service.ManagedFileRepository {
	return NewManagedFileRepository(db)
}
func ProvideFileRepositoryWithManagedLifecycle(db *sql.DB) service.FileRepository {
	return NewFileRepositoryWithRemoteLifecycle(db, NewManagedFileRepository(db))
}
