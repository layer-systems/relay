# NIP-56 Reports Implementation

This relay implements [NIP-56 (Reporting)](https://nips.nostr.com/) for handling content reports and integrates them with the [NIP-86 Management API](https://nips.nostr.com/86).

## Features

### Report Storage
- Automatically stores all kind `1984` events (reports) to a dedicated database table
- Extracts report metadata:
  - Reporter pubkey
  - Reported pubkey (from `p` tag)
  - Reported event ID (from `e` tag, optional)
  - Report type (nudity, malware, profanity, illegal, spam, impersonation, other)
  - Report content/reason

### Report Management via NIP-86

The following NIP-86 management methods are available:

#### `listevents needingmoderation`
Returns all unresolved reports showing:
- Event ID or pubkey that was reported
- Report type, content, and reporter information
- Only shows reports that haven't been resolved yet

#### `allowevent`
- Marks all reports about the specified event/pubkey as resolved
- Adds the event to the `allowed_events` table
- Useful for dismissing false reports

#### `banevent`
- Marks all reports about the specified event/pubkey as resolved
- Adds the event to the `banned_events` table
- Deletes the actual event from the relay (if it exists)
- Prevents the event from being accepted in the future

#### `listbannedevents`
Returns all events that have been banned, with reasons

#### `listaLlowedevents`
Returns all events that have been explicitly allowed (reports dismissed)

### Event Rejection
The relay automatically rejects:
- Events from banned pubkeys
- Events with IDs in the banned events list

## Database Schema

### `reports` table
```sql
CREATE TABLE reports (
    id TEXT PRIMARY KEY,              -- Report event ID
    reporter_pubkey TEXT NOT NULL,    -- Who submitted the report
    reported_event_id TEXT,           -- Event being reported (optional)
    reported_pubkey TEXT NOT NULL,    -- Pubkey being reported
    report_type TEXT NOT NULL,        -- nudity, malware, profanity, etc.
    content TEXT,                     -- Additional details from reporter
    resolved BOOLEAN DEFAULT FALSE,   -- Whether report has been handled
    resolution TEXT,                  -- How it was resolved
    created_at TIMESTAMP DEFAULT NOW()
)
```

### `banned_events` table
```sql
CREATE TABLE banned_events (
    event_id TEXT PRIMARY KEY,
    reason TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
)
```

### `allowed_events` table
```sql
CREATE TABLE allowed_events (
    event_id TEXT PRIMARY KEY,
    reason TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
)
```

## Usage Example

### Viewing Reports
Use a NIP-86 compatible client to call:
```
listeventsNeedingModeration
```

### Handling a Report
Ban a reported event:
```
banEvent("event_id_here", "confirmed spam")
```

Or dismiss a false report:
```
allowEvent("event_id_here", "false report - content is acceptable")
```

## Report Types (NIP-56)

- `nudity` - depictions of nudity, porn, etc.
- `malware` - virus, trojan horse, worm, etc.
- `profanity` - profanity, hateful speech, etc.
- `illegal` - something which may be illegal in some jurisdiction
- `spam` - spam
- `impersonation` - someone pretending to be someone else
- `other` - for reports that don't fit in the above categories

## Authentication

All NIP-86 management endpoints require authentication with the relay owner's pubkey (configured via `RELAY_PUBKEY` environment variable).
