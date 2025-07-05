package d1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// ChatSettingsStore implementation

const (
	putChatSettingQuery = `
		INSERT INTO whatsmeow_chat_settings (our_jid, chat_jid, %s) VALUES (?, ?, ?)
		ON CONFLICT (our_jid, chat_jid) DO UPDATE SET %s=excluded.%s
	`
	getChatSettingsQuery = `
		SELECT muted_until, pinned, archived FROM whatsmeow_chat_settings WHERE our_jid=? AND chat_jid=?
	`
)

func (s *D1Store) PutMutedUntil(ctx context.Context, chat types.JID, mutedUntil time.Time) error {
	var val int64
	if !mutedUntil.IsZero() {
		val = mutedUntil.Unix()
	}
	query := fmt.Sprintf(putChatSettingQuery, "muted_until", "muted_until", "muted_until")
	_, err := s.db().Exec(ctx, query, s.JID, chat, val)
	return err
}

func (s *D1Store) PutPinned(ctx context.Context, chat types.JID, pinned bool) error {
	query := fmt.Sprintf(putChatSettingQuery, "pinned", "pinned", "pinned")
	_, err := s.db().Exec(ctx, query, s.JID, chat, pinned)
	return err
}

func (s *D1Store) PutArchived(ctx context.Context, chat types.JID, archived bool) error {
	query := fmt.Sprintf(putChatSettingQuery, "archived", "archived", "archived")
	_, err := s.db().Exec(ctx, query, s.JID, chat, archived)
	return err
}

func (s *D1Store) GetChatSettings(ctx context.Context, chat types.JID) (settings types.LocalChatSettings, err error) {
	var mutedUntil int64
	err = s.db().QueryRow(ctx, getChatSettingsQuery, s.JID, chat).Scan(&mutedUntil, &settings.Pinned, &settings.Archived)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err != nil {
		return
	} else {
		settings.Found = true
	}
	if mutedUntil != 0 {
		settings.MutedUntil = time.Unix(mutedUntil, 0)
	}
	return
}

// MsgSecretStore implementation

const (
	putMsgSecret = `
		INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (our_jid, chat_jid, sender_jid, message_id) DO NOTHING
	`
	getMsgSecret = `
		SELECT key FROM whatsmeow_message_secrets WHERE our_jid=? AND chat_jid=? AND sender_jid=? AND message_id=?
	`
)

func (s *D1Store) PutMessageSecrets(ctx context.Context, inserts []store.MessageSecretInsert) (err error) {
	if len(inserts) == 0 {
		return nil
	}
	return s.db().DoTxn(ctx, nil, func(ctx context.Context) error {
		for _, insert := range inserts {
			_, err = s.db().Exec(ctx, putMsgSecret, s.JID, insert.Chat.ToNonAD(), insert.Sender.ToNonAD(), insert.ID, insert.Secret)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *D1Store) PutMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID, secret []byte) (err error) {
	_, err = s.db().Exec(ctx, putMsgSecret, s.JID, chat.ToNonAD(), sender.ToNonAD(), id, secret)
	return
}

func (s *D1Store) GetMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID) (secret []byte, err error) {
	err = s.db().QueryRow(ctx, getMsgSecret, s.JID, chat.ToNonAD(), sender.ToNonAD(), id).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

// PrivacyTokenStore implementation

const (
	putPrivacyTokens = `
		INSERT INTO whatsmeow_privacy_tokens (our_jid, their_jid, token, timestamp)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (our_jid, their_jid) DO UPDATE SET token=excluded.token, timestamp=excluded.timestamp
	`
	getPrivacyToken = `SELECT token, timestamp FROM whatsmeow_privacy_tokens WHERE our_jid=? AND their_jid=?`
)

func (s *D1Store) PutPrivacyTokens(ctx context.Context, tokens ...store.PrivacyToken) error {
	if len(tokens) == 0 {
		return nil
	}

	// Build batch insert query
	valuesParts := make([]string, len(tokens))
	args := make([]any, len(tokens)*4)

	for i, token := range tokens {
		valuesParts[i] = "(?, ?, ?, ?)"
		baseIndex := i * 4
		args[baseIndex] = s.JID
		args[baseIndex+1] = token.User.ToNonAD().String()
		args[baseIndex+2] = token.Token
		args[baseIndex+3] = token.Timestamp.Unix()
	}

	query := strings.ReplaceAll(putPrivacyTokens, "(?, ?, ?, ?)", strings.Join(valuesParts, ","))
	_, err := s.db().Exec(ctx, query, args...)
	return err
}

func (s *D1Store) GetPrivacyToken(ctx context.Context, user types.JID) (*store.PrivacyToken, error) {
	var token store.PrivacyToken
	token.User = user.ToNonAD()
	var ts int64
	err := s.db().QueryRow(ctx, getPrivacyToken, s.JID, token.User).Scan(&token.Token, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else {
		token.Timestamp = time.Unix(ts, 0)
		return &token, nil
	}
}

// EventBuffer implementation

const (
	getBufferedEventQuery = `
		SELECT plaintext, server_timestamp, insert_timestamp FROM whatsmeow_event_buffer WHERE our_jid = ? AND ciphertext_hash = ?
	`
	putBufferedEventQuery = `
		INSERT INTO whatsmeow_event_buffer (our_jid, ciphertext_hash, plaintext, server_timestamp, insert_timestamp)
		VALUES (?, ?, ?, ?, ?)
	`
	clearBufferedEventPlaintextQuery = `
		UPDATE whatsmeow_event_buffer SET plaintext = NULL WHERE our_jid = ? AND ciphertext_hash = ?
	`
	deleteOldBufferedHashesQuery = `
		DELETE FROM whatsmeow_event_buffer WHERE insert_timestamp < ?
	`
)

func (s *D1Store) GetBufferedEvent(ctx context.Context, ciphertextHash [32]byte) (*store.BufferedEvent, error) {
	var insertTimeMS, serverTimeSeconds int64
	var buf store.BufferedEvent
	err := s.db().QueryRow(ctx, getBufferedEventQuery, s.JID, ciphertextHash[:]).Scan(&buf.Plaintext, &serverTimeSeconds, &insertTimeMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	buf.ServerTime = time.Unix(serverTimeSeconds, 0)
	buf.InsertTime = time.UnixMilli(insertTimeMS)
	return &buf, nil
}

func (s *D1Store) PutBufferedEvent(ctx context.Context, ciphertextHash [32]byte, plaintext []byte, serverTimestamp time.Time) error {
	_, err := s.db().Exec(ctx, putBufferedEventQuery, s.JID, ciphertextHash[:], plaintext, serverTimestamp.Unix(), time.Now().UnixMilli())
	return err
}

func (s *D1Store) DoDecryptionTxn(ctx context.Context, fn func(context.Context) error) error {
	return s.db().DoTxn(ctx, nil, fn)
}

func (s *D1Store) ClearBufferedEventPlaintext(ctx context.Context, ciphertextHash [32]byte) error {
	_, err := s.db().Exec(ctx, clearBufferedEventPlaintextQuery, s.JID, ciphertextHash[:])
	return err
}

func (s *D1Store) DeleteOldBufferedHashes(ctx context.Context) error {
	// The WhatsApp servers only buffer events for 14 days,
	// so we can safely delete anything older than that.
	_, err := s.db().Exec(ctx, deleteOldBufferedHashesQuery, time.Now().Add(-14*24*time.Hour).UnixMilli())
	return err
}

// LIDStore implementation (Global store)

const (
	putLIDMappingQuery     = `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?) ON CONFLICT (lid) DO UPDATE SET pn=excluded.pn`
	putManyLIDMappingsBase = `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES `
	getLIDForPNQuery       = `SELECT lid FROM whatsmeow_lid_map WHERE pn=?`
	getPNForLIDQuery       = `SELECT pn FROM whatsmeow_lid_map WHERE lid=?`
)

func (s *D1Store) PutLIDMapping(ctx context.Context, lid, pn types.JID) error {
	_, err := s.db().Exec(ctx, putLIDMappingQuery, lid.String(), pn.String())
	return err
}

func (s *D1Store) PutManyLIDMappings(ctx context.Context, mappings []store.LIDMapping) error {
	if len(mappings) == 0 {
		return nil
	}

	valuesParts := make([]string, len(mappings))
	args := make([]any, len(mappings)*2)

	for i, mapping := range mappings {
		valuesParts[i] = "(?, ?)"
		baseIndex := i * 2
		args[baseIndex] = mapping.LID.String()
		args[baseIndex+1] = mapping.PN.String()
	}

	query := putManyLIDMappingsBase + strings.Join(valuesParts, ",") + " ON CONFLICT (lid) DO UPDATE SET pn=excluded.pn"
	_, err := s.db().Exec(ctx, query, args...)
	return err
}

func (s *D1Store) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	var lidStr string
	err := s.db().QueryRow(ctx, getLIDForPNQuery, pn.String()).Scan(&lidStr)
	if errors.Is(err, sql.ErrNoRows) {
		return types.EmptyJID, nil
	} else if err != nil {
		return types.EmptyJID, err
	}
	lid, err := types.ParseJID(lidStr)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to parse LID from database: %w", err)
	}
	return lid, nil
}

func (s *D1Store) GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error) {
	var pnStr string
	err := s.db().QueryRow(ctx, getPNForLIDQuery, lid.String()).Scan(&pnStr)
	if errors.Is(err, sql.ErrNoRows) {
		return types.EmptyJID, nil
	} else if err != nil {
		return types.EmptyJID, err
	}
	pn, err := types.ParseJID(pnStr)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to parse PN from database: %w", err)
	}
	return pn, nil
}
