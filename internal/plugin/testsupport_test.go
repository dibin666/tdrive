package plugin

import (
	"context"
	"testing"

	"github.com/dibin/tdrive/internal/database"
)

// newTestUser creates an account for a plugin to belong to.
//
// Plugins are owned per account and plugins.user_id is a foreign key into
// users, so a test that inserts a plugin row has to insert an owner first.
// Every plugin test needs the same two lines, so they live here once.
func newTestUser(t *testing.T, db *database.DB, username string) database.User {
	t.Helper()
	user, err := db.CreateUser(context.Background(), username, "hash", database.RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", username, err)
	}
	return user
}
