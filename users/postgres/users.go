// Copyright (c) Mainflux
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/mainflux/mainflux/auth"
	"github.com/mainflux/mainflux/pkg/errors"
	"github.com/mainflux/mainflux/users"
)

const (
	errInvalid    = "invalid_text_representation"
	errTruncation = "string_data_right_truncation"
)

var _ users.UserRepository = (*userRepository)(nil)

const errDuplicate = "unique_violation"

type userRepository struct {
	db Database
}

// NewUserRepo instantiates a PostgreSQL implementation of user
// repository.
func NewUserRepo(db Database) users.UserRepository {
	return &userRepository{
		db: db,
	}
}

func (ur userRepository) Save(ctx context.Context, user users.User) (string, error) {
	q := `INSERT INTO users (email, password, id, metadata, status) VALUES (:email, :password, :id, :metadata, :status) RETURNING id`
	if user.ID == "" || user.Email == "" {
		return "", errors.ErrMalformedEntity
	}

	dbu, err := toDBUser(user)
	if err != nil {
		return "", errors.Wrap(errors.ErrCreateEntity, err)
	}

	row, err := ur.db.NamedQueryContext(ctx, q, dbu)
	if err != nil {
		pqErr, ok := err.(*pq.Error)
		if ok {
			switch pqErr.Code.Name() {
			case errInvalid, errTruncation:
				return "", errors.Wrap(errors.ErrMalformedEntity, err)
			case errDuplicate:
				return "", errors.Wrap(errors.ErrConflict, err)
			}
		}
		return "", errors.Wrap(errors.ErrCreateEntity, err)
	}

	defer row.Close()
	row.Next()
	var id string
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (ur userRepository) Update(ctx context.Context, user users.User) error {
	q := `UPDATE users SET(email, password, metadata, status) VALUES (:email, :password, :metadata, :status) WHERE email = :email;`

	dbu, err := toDBUser(user)
	if err != nil {
		return errors.Wrap(errors.ErrUpdateEntity, err)
	}

	if _, err := ur.db.NamedExecContext(ctx, q, dbu); err != nil {
		return errors.Wrap(errors.ErrUpdateEntity, err)
	}

	return nil
}

func (ur userRepository) UpdateUser(ctx context.Context, user users.User) error {
	q := `UPDATE users SET metadata = :metadata WHERE email = :email AND status = 'enabled'`

	dbu, err := toDBUser(user)
	if err != nil {
		return errors.Wrap(errors.ErrUpdateEntity, err)
	}

	if _, err := ur.db.NamedExecContext(ctx, q, dbu); err != nil {
		return errors.Wrap(errors.ErrUpdateEntity, err)
	}

	return nil
}

func (ur userRepository) RetrieveByEmail(ctx context.Context, email string) (users.User, error) {
	q := `SELECT id, password, metadata FROM users WHERE email = $1 AND status = 'enabled'`

	dbu := dbUser{
		Email: email,
	}

	if err := ur.db.QueryRowxContext(ctx, q, email).StructScan(&dbu); err != nil {
		if err == sql.ErrNoRows {
			return users.User{}, errors.Wrap(errors.ErrNotFound, err)

		}
		return users.User{}, errors.Wrap(errors.ErrViewEntity, err)
	}

	return toUser(dbu)
}

func (ur userRepository) RetrieveByID(ctx context.Context, id string) (users.User, error) {
	q := `SELECT email, password, metadata FROM users WHERE id = $1`

	dbu := dbUser{
		ID: id,
	}

	if err := ur.db.QueryRowxContext(ctx, q, id).StructScan(&dbu); err != nil {
		if err == sql.ErrNoRows {
			return users.User{}, errors.Wrap(errors.ErrNotFound, err)

		}
		return users.User{}, errors.Wrap(errors.ErrViewEntity, err)
	}

	return toUser(dbu)
}

func (ur userRepository) RetrieveAll(ctx context.Context, status string, offset, limit uint64, userIDs []string, email string, um users.Metadata) (users.UserPage, error) {
	eq, ep, err := createEmailQuery("", email)
	if err != nil {
		return users.UserPage{}, errors.Wrap(errors.ErrViewEntity, err)
	}

	mq, mp, err := createMetadataQuery("", um)
	if err != nil {
		return users.UserPage{}, errors.Wrap(errors.ErrViewEntity, err)
	}
	aq := fmt.Sprintf("status = '%s'", status)
	if status == users.AllStatusKey {
		aq = ""
	}

	var query []string
	var emq string
	if eq != "" {
		query = append(query, eq)
	}
	if mq != "" {
		query = append(query, mq)
	}
	if aq != "" {
		query = append(query, aq)
	}

	if len(userIDs) > 0 {
		query = append(query, fmt.Sprintf("id IN ('%s')", strings.Join(userIDs, "','")))
	}
	if len(query) > 0 {
		emq = fmt.Sprintf(" WHERE %s", strings.Join(query, " AND "))
	}

	q := fmt.Sprintf(`SELECT id, email, metadata FROM users %s ORDER BY email LIMIT :limit OFFSET :offset;`, emq)
	params := map[string]interface{}{
		"limit":    limit,
		"offset":   offset,
		"email":    ep,
		"metadata": mp,
	}

	rows, err := ur.db.NamedQueryContext(ctx, q, params)
	if err != nil {
		return users.UserPage{}, errors.Wrap(errors.ErrViewEntity, err)
	}
	defer rows.Close()

	var items []users.User
	for rows.Next() {
		dbusr := dbUser{}
		if err := rows.StructScan(&dbusr); err != nil {
			return users.UserPage{}, errors.Wrap(errors.ErrViewEntity, err)
		}

		user, err := toUser(dbusr)
		if err != nil {
			return users.UserPage{}, err
		}

		items = append(items, user)
	}

	cq := fmt.Sprintf(`SELECT COUNT(*) FROM users %s;`, emq)

	total, err := total(ctx, ur.db, cq, params)
	if err != nil {
		return users.UserPage{}, errors.Wrap(errors.ErrViewEntity, err)
	}

	page := users.UserPage{
		Users: items,
		PageMetadata: users.PageMetadata{
			Total:  total,
			Offset: offset,
			Limit:  limit,
		},
	}

	return page, nil
}

func (ur userRepository) UpdatePassword(ctx context.Context, email, password string) error {
	q := `UPDATE users SET password = :password WHERE status = 'enabled' AND email = :email`

	db := dbUser{
		Email:    email,
		Password: password,
	}

	if _, err := ur.db.NamedExecContext(ctx, q, db); err != nil {
		return errors.Wrap(errors.ErrUpdateEntity, err)
	}

	return nil
}

func (ur userRepository) ChangeStatus(ctx context.Context, id, status string) error {
	q := fmt.Sprintf(`UPDATE users SET status = '%s' WHERE id = :id`, status)

	dbu := dbUser{
		ID: id,
	}

	if _, err := ur.db.NamedExecContext(ctx, q, dbu); err != nil {
		return errors.Wrap(errors.ErrUpdateEntity, err)
	}

	return nil
}

type dbUser struct {
	ID       string       `db:"id"`
	Email    string       `db:"email"`
	Password string       `db:"password"`
	Metadata []byte       `db:"metadata"`
	Groups   []auth.Group `db:"groups"`
	Status   string       `db:"status"`
}

func toDBUser(u users.User) (dbUser, error) {
	data := []byte("{}")
	if len(u.Metadata) > 0 {
		b, err := json.Marshal(u.Metadata)
		if err != nil {
			return dbUser{}, errors.Wrap(errors.ErrMalformedEntity, err)
		}
		data = b
	}

	return dbUser{
		ID:       u.ID,
		Email:    u.Email,
		Password: u.Password,
		Metadata: data,
		Status:   u.Status,
	}, nil
}

func total(ctx context.Context, db Database, query string, params interface{}) (uint64, error) {
	rows, err := db.NamedQueryContext(ctx, query, params)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	total := uint64(0)
	if rows.Next() {
		if err := rows.Scan(&total); err != nil {
			return 0, err
		}
	}
	return total, nil
}

func toUser(dbu dbUser) (users.User, error) {
	var metadata map[string]interface{}
	if dbu.Metadata != nil {
		if err := json.Unmarshal([]byte(dbu.Metadata), &metadata); err != nil {
			return users.User{}, errors.Wrap(errors.ErrMalformedEntity, err)
		}
	}

	return users.User{
		ID:       dbu.ID,
		Email:    dbu.Email,
		Password: dbu.Password,
		Metadata: metadata,
		Status:   dbu.Status,
	}, nil
}

func createEmailQuery(entity string, email string) (string, string, error) {
	if email == "" {
		return "", "", nil
	}

	// Create LIKE operator to search Users with email containing a given string
	param := fmt.Sprintf(`%%%s%%`, email)
	query := fmt.Sprintf("%semail LIKE :email", entity)

	return query, param, nil
}

func createMetadataQuery(entity string, um users.Metadata) (string, []byte, error) {
	if len(um) == 0 {
		return "", nil, nil
	}

	param, err := json.Marshal(um)
	if err != nil {
		return "", nil, err
	}
	query := fmt.Sprintf("%smetadata @> :metadata", entity)

	return query, param, nil
}
