package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// User represents a user in the system.
type User struct {
	ID           int       `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"` // never expose in API responses
	Role         string    `json:"role"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GetUserByUsername looks up a user by username for login.
func (db *DB) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash, role, enabled, created_at, updated_at
		 FROM users WHERE username = $1`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID looks up a user by ID for JWT validation.
func (db *DB) GetUserByID(ctx context.Context, id int) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash, role, enabled, created_at, updated_at
		 FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers returns all users (for admin endpoint).
func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := db.Pool.Query(ctx,
		`SELECT id, username, password_hash, role, enabled, created_at, updated_at
		 FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CreateUser inserts a new user with an already-hashed password.
func (db *DB) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, role)
		 VALUES ($1, $2, $3)
		 RETURNING id, username, password_hash, role, enabled, created_at, updated_at`,
		username, passwordHash, role,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UserUpdate holds optional fields for a partial user update.
type UserUpdate struct {
	Role         *string
	PasswordHash *string
	Enabled      *bool
}

// UpdateUser applies a partial update to an existing user.
func (db *DB) UpdateUser(ctx context.Context, id int, upd UserUpdate) (*User, error) {
	var u User
	err := db.Pool.QueryRow(ctx,
		`UPDATE users SET
			role = COALESCE($2, role),
			password_hash = COALESCE($3, password_hash),
			enabled = COALESCE($4, enabled)
		 WHERE id = $1
		 RETURNING id, username, password_hash, role, enabled, created_at, updated_at`,
		id, upd.Role, upd.PasswordHash, upd.Enabled,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// DeleteUser removes a user by ID.
func (db *DB) DeleteUser(ctx context.Context, id int) error {
	ct, err := db.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountUsers returns the total number of users (used for seeding check).
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count)
	return count, err
}
