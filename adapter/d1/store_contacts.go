package d1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// ContactStore implementation

const (
	putContactNameQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, first_name, full_name) VALUES (?, ?, ?, ?)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET first_name=excluded.first_name, full_name=excluded.full_name
	`
	putManyContactNamesQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, first_name, full_name)
		VALUES %s
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET first_name=excluded.first_name, full_name=excluded.full_name
	`
	putPushNameQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES (?, ?, ?)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET push_name=excluded.push_name
	`
	putBusinessNameQuery = `
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, business_name) VALUES (?, ?, ?)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET business_name=excluded.business_name
	`
	getContactQuery = `
		SELECT first_name, full_name, push_name, business_name FROM whatsmeow_contacts WHERE our_jid=? AND their_jid=?
	`
	getAllContactsQuery = `
		SELECT their_jid, first_name, full_name, push_name, business_name FROM whatsmeow_contacts WHERE our_jid=?
	`
)

func (s *D1Store) PutPushName(ctx context.Context, user types.JID, pushName string) (bool, string, error) {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()

	cached, err := s.getContact(ctx, user)
	if err != nil {
		return false, "", err
	}
	if cached.PushName != pushName {
		_, err = s.db().Exec(ctx, putPushNameQuery, s.JID, user, pushName)
		if err != nil {
			return false, "", err
		}
		previousName := cached.PushName
		cached.PushName = pushName
		cached.Found = true
		return true, previousName, nil
	}
	return false, "", nil
}

func (s *D1Store) PutBusinessName(ctx context.Context, user types.JID, businessName string) (bool, string, error) {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()

	cached, err := s.getContact(ctx, user)
	if err != nil {
		return false, "", err
	}
	if cached.BusinessName != businessName {
		_, err = s.db().Exec(ctx, putBusinessNameQuery, s.JID, user, businessName)
		if err != nil {
			return false, "", err
		}
		previousName := cached.BusinessName
		cached.BusinessName = businessName
		cached.Found = true
		return true, previousName, nil
	}
	return false, "", nil
}

func (s *D1Store) PutContactName(ctx context.Context, user types.JID, firstName, fullName string) error {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()

	cached, err := s.getContact(ctx, user)
	if err != nil {
		return err
	}
	if cached.FirstName != firstName || cached.FullName != fullName {
		_, err = s.db().Exec(ctx, putContactNameQuery, s.JID, user, firstName, fullName)
		if err != nil {
			return err
		}
		cached.FirstName = firstName
		cached.FullName = fullName
		cached.Found = true
	}
	return nil
}

const contactBatchSize = 300

func (s *D1Store) putContactNamesBatch(ctx context.Context, contacts []store.ContactEntry) error {
	values := make([]any, 1, 1+len(contacts)*3)
	queryParts := make([]string, 0, len(contacts))
	values[0] = s.JID
	placeholderSyntax := "(?, ?, ?, ?)"
	i := 0
	handledContacts := make(map[types.JID]struct{}, len(contacts))
	for _, contact := range contacts {
		if contact.JID.IsEmpty() {
			s.log.Warnf("Empty contact info in mass insert: %+v", contact)
			continue
		}
		// The whole query will break if there are duplicates, so make sure there aren't any duplicates
		_, alreadyHandled := handledContacts[contact.JID]
		if alreadyHandled {
			s.log.Warnf("Duplicate contact info for %s in mass insert", contact.JID)
			continue
		}
		handledContacts[contact.JID] = struct{}{}
		values = append(values, s.JID, contact.JID.String(), contact.FirstName, contact.FullName)
		queryParts = append(queryParts, placeholderSyntax)
		i++
	}
	query := fmt.Sprintf(putManyContactNamesQuery, strings.Join(queryParts, ","))

	// Flatten values for the final query
	finalValues := make([]any, len(queryParts)*4)
	for i := 0; i < len(queryParts); i++ {
		baseIndex := i * 4
		contactIndex := i + 1 // Skip the first s.JID entry
		finalValues[baseIndex] = s.JID
		finalValues[baseIndex+1] = values[contactIndex*3+1] // JID
		finalValues[baseIndex+2] = values[contactIndex*3+2] // FirstName
		finalValues[baseIndex+3] = values[contactIndex*3+3] // FullName
	}

	_, err := s.db().Exec(ctx, query, finalValues...)
	return err
}

func (s *D1Store) PutAllContactNames(ctx context.Context, contacts []store.ContactEntry) error {
	if len(contacts) == 0 {
		return nil
	}
	err := s.db().DoTxn(ctx, nil, func(ctx context.Context) error {
		for i := 0; i < len(contacts); i += contactBatchSize {
			end := i + contactBatchSize
			if end > len(contacts) {
				end = len(contacts)
			}
			slice := contacts[i:end]
			err := s.putContactNamesBatch(ctx, slice)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.contactCacheLock.Lock()
	// Just clear the cache, fetching pushnames and business names would be too much effort
	s.contactCache = make(map[types.JID]*types.ContactInfo)
	s.contactCacheLock.Unlock()
	return nil
}

func (s *D1Store) getContact(ctx context.Context, user types.JID) (*types.ContactInfo, error) {
	cached, ok := s.contactCache[user]
	if ok {
		return cached, nil
	}

	var first, full, push, business sql.NullString
	err := s.db().QueryRow(ctx, getContactQuery, s.JID, user).Scan(&first, &full, &push, &business)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	info := &types.ContactInfo{
		Found:        err == nil,
		FirstName:    first.String,
		FullName:     full.String,
		PushName:     push.String,
		BusinessName: business.String,
	}
	s.contactCache[user] = info
	return info, nil
}

func (s *D1Store) GetContact(ctx context.Context, user types.JID) (types.ContactInfo, error) {
	s.contactCacheLock.Lock()
	info, err := s.getContact(ctx, user)
	s.contactCacheLock.Unlock()
	if err != nil {
		return types.ContactInfo{}, err
	}
	return *info, nil
}

func (s *D1Store) GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	s.contactCacheLock.Lock()
	defer s.contactCacheLock.Unlock()
	rows, err := s.db().Query(ctx, getAllContactsQuery, s.JID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	output := make(map[types.JID]types.ContactInfo, len(s.contactCache))
	for rows.Next() {
		var jid types.JID
		var first, full, push, business sql.NullString
		err = rows.Scan(&jid, &first, &full, &push, &business)
		if err != nil {
			return nil, fmt.Errorf("error scanning row: %w", err)
		}
		info := types.ContactInfo{
			Found:        true,
			FirstName:    first.String,
			FullName:     full.String,
			PushName:     push.String,
			BusinessName: business.String,
		}
		output[jid] = info
		s.contactCache[jid] = &info
	}
	return output, nil
}
