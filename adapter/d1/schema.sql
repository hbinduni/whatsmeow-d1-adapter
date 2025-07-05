-- Cloudflare D1 Schema for Whatsmeow
-- Based on whatsmeow/store/sqlstore/upgrades/00-latest-schema.sql

-- Disable foreign key constraints for now
PRAGMA foreign_keys = OFF;

-- Drop existing tables to ensure clean slate
DROP TABLE IF EXISTS whatsmeow_event_buffer;
DROP TABLE IF EXISTS whatsmeow_message_secrets;
DROP TABLE IF EXISTS whatsmeow_chat_settings;
DROP TABLE IF EXISTS whatsmeow_contacts;
DROP TABLE IF EXISTS whatsmeow_app_state_mutation_macs;
DROP TABLE IF EXISTS whatsmeow_app_state_version;
DROP TABLE IF EXISTS whatsmeow_app_state_sync_keys;
DROP TABLE IF EXISTS whatsmeow_sender_keys;
DROP TABLE IF EXISTS whatsmeow_sessions;
DROP TABLE IF EXISTS whatsmeow_pre_keys;
DROP TABLE IF EXISTS whatsmeow_identity_keys;
DROP TABLE IF EXISTS whatsmeow_privacy_tokens;
DROP TABLE IF EXISTS whatsmeow_lid_map;
DROP TABLE IF EXISTS whatsmeow_device;

CREATE TABLE IF NOT EXISTS whatsmeow_device (
    jid TEXT PRIMARY KEY,
    lid TEXT,
    
    facebook_uuid TEXT,
    
    registration_id INTEGER NOT NULL CHECK ( registration_id >= 0 AND registration_id < 4294967296 ),
    
    noise_key BLOB NOT NULL CHECK ( length(noise_key) = 32 ),
    identity_key BLOB NOT NULL CHECK ( length(identity_key) = 32 ),
    
    signed_pre_key BLOB NOT NULL CHECK ( length(signed_pre_key) = 32 ),
    signed_pre_key_id INTEGER NOT NULL CHECK ( signed_pre_key_id >= 0 AND signed_pre_key_id < 16777216 ),
    signed_pre_key_sig BLOB NOT NULL CHECK ( length(signed_pre_key_sig) = 64 ),
    
    adv_key BLOB NOT NULL,
    adv_details BLOB NOT NULL,
    adv_account_sig BLOB NOT NULL CHECK ( length(adv_account_sig) = 64 ),
    adv_account_sig_key BLOB NOT NULL CHECK ( length(adv_account_sig_key) = 32 ),
    adv_device_sig BLOB NOT NULL CHECK ( length(adv_device_sig) = 64 ),
    
    platform TEXT NOT NULL DEFAULT '',
    business_name TEXT NOT NULL DEFAULT '',
    push_name TEXT NOT NULL DEFAULT '',
    
    lid_migration_ts INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS whatsmeow_identity_keys (
    our_jid TEXT,
    their_id TEXT,
    identity BLOB NOT NULL CHECK ( length(identity) = 32 ),
    
    PRIMARY KEY (our_jid, their_id)
);

CREATE TABLE IF NOT EXISTS whatsmeow_pre_keys (
    jid TEXT,
    key_id INTEGER CHECK ( key_id >= 0 AND key_id < 16777216 ),
    key BLOB NOT NULL CHECK ( length(key) = 32 ),
    uploaded BOOLEAN NOT NULL,
    
    PRIMARY KEY (jid, key_id)
);

CREATE TABLE IF NOT EXISTS whatsmeow_sessions (
    our_jid TEXT,
    their_id TEXT,
    session BLOB,
    
    PRIMARY KEY (our_jid, their_id)
);

CREATE TABLE IF NOT EXISTS whatsmeow_sender_keys (
    our_jid TEXT,
    chat_id TEXT,
    sender_id TEXT,
    sender_key BLOB NOT NULL,
    
    PRIMARY KEY (our_jid, chat_id, sender_id)
);

CREATE TABLE IF NOT EXISTS whatsmeow_app_state_sync_keys (
    jid TEXT,
    key_id BLOB,
    key_data BLOB NOT NULL,
    timestamp INTEGER NOT NULL,
    fingerprint BLOB NOT NULL,
    
    PRIMARY KEY (jid, key_id)
);

CREATE TABLE IF NOT EXISTS whatsmeow_app_state_version (
    jid TEXT,
    name TEXT,
    version INTEGER NOT NULL,
    hash BLOB NOT NULL CHECK ( length(hash) = 128 ),
    
    PRIMARY KEY (jid, name)
);

CREATE TABLE IF NOT EXISTS whatsmeow_app_state_mutation_macs (
    jid TEXT,
    name TEXT,
    version INTEGER,
    index_mac BLOB CHECK ( length(index_mac) = 32 ),
    value_mac BLOB NOT NULL CHECK ( length(value_mac) = 32 ),
    
    PRIMARY KEY (jid, name, version, index_mac)
);

CREATE TABLE IF NOT EXISTS whatsmeow_contacts (
    our_jid TEXT,
    their_jid TEXT,
    first_name TEXT,
    full_name TEXT,
    push_name TEXT,
    business_name TEXT,
    
    PRIMARY KEY (our_jid, their_jid)
);

CREATE TABLE IF NOT EXISTS whatsmeow_chat_settings (
    our_jid TEXT,
    chat_jid TEXT,
    muted_until INTEGER NOT NULL DEFAULT 0,
    pinned BOOLEAN NOT NULL DEFAULT false,
    archived BOOLEAN NOT NULL DEFAULT false,
    
    PRIMARY KEY (our_jid, chat_jid)
);

CREATE TABLE IF NOT EXISTS whatsmeow_message_secrets (
    our_jid TEXT,
    chat_jid TEXT,
    sender_jid TEXT,
    message_id TEXT,
    key BLOB NOT NULL,
    
    PRIMARY KEY (our_jid, chat_jid, sender_jid, message_id)
);

CREATE TABLE IF NOT EXISTS whatsmeow_privacy_tokens (
    our_jid TEXT,
    their_jid TEXT,
    token BLOB NOT NULL,
    timestamp INTEGER NOT NULL,
    PRIMARY KEY (our_jid, their_jid)
);

CREATE TABLE IF NOT EXISTS whatsmeow_lid_map (
    lid TEXT PRIMARY KEY,
    pn TEXT UNIQUE NOT NULL
);

CREATE TABLE IF NOT EXISTS whatsmeow_event_buffer (
    our_jid TEXT NOT NULL,
    ciphertext_hash BLOB NOT NULL CHECK ( length(ciphertext_hash) = 32 ),
    plaintext BLOB,
    server_timestamp INTEGER NOT NULL,
    insert_timestamp INTEGER NOT NULL,
    PRIMARY KEY (our_jid, ciphertext_hash)
);