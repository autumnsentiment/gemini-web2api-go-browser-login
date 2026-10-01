package app

import "testing"

func TestParsePageIdentity(t *testing.T) {
	body := []byte(`x "SNlM0e":"AbCdEfGhIjKlMn" "oPEP7c":"Second@Example.test" "QrtxK":"1" y`)
	id := parsePageIdentity(body)
	if !id.LoggedIn || id.Email != "second@example.test" || id.Slot != 1 {
		t.Fatalf("parse = %+v", id)
	}
	anon := parsePageIdentity([]byte(`"QrtxK":"" "S06Grb":""`))
	if anon.LoggedIn || anon.Email != "" || anon.Slot != -1 {
		t.Fatalf("anon parse = %+v", anon)
	}
}

func TestCheckSlotIdentity(t *testing.T) {
	ok := pageIdentity{Email: "second@example.test", Slot: 1, LoggedIn: true}
	if _, err := checkSlotIdentity(ok, 1, "SECOND@example.test"); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if _, err := checkSlotIdentity(ok, 1, ""); err != nil {
		t.Fatalf("legacy client without email rejected: %v", err)
	}
	if _, err := checkSlotIdentity(ok, 1, "first@example.test"); err == nil {
		t.Fatal("wrong account accepted")
	}
	if _, err := checkSlotIdentity(ok, 2, "second@example.test"); err == nil {
		t.Fatal("slot mismatch accepted")
	}
	if _, err := checkSlotIdentity(pageIdentity{Slot: -1}, 0, "first@example.test"); err == nil {
		t.Fatal("signed-out page accepted")
	}
	if _, err := checkSlotIdentity(pageIdentity{Slot: 0, LoggedIn: true}, 0, "first@example.test"); err == nil {
		t.Fatal("missing page email accepted")
	}
}

func TestPurgeLegacySlotProfiles(t *testing.T) {
	db := getDB()
	keep, err := insertID(`INSERT INTO accounts(label, cookie, status, note, created_at, source, profile, authuser) VALUES ('k','SAPISID=a','enabled','',0,'remote','browser1',1)`)
	if err != nil {
		t.Fatal(err)
	}
	ghost, err := insertID(`INSERT INTO accounts(label, cookie, status, note, created_at, source, profile, authuser) VALUES ('g','SAPISID=a','enabled','',0,'remote','browser1-u3',3)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM accounts WHERE id IN (?,?)`, keep, ghost)
		_, _ = db.Exec(`DELETE FROM kv WHERE k LIKE 'ext_seen_at:%' OR k LIKE 'browser_next_refresh:%' OR k='other:acct1-u2'`)
	})
	for _, k := range []string{"ext_seen_at:browser1-u3", "browser_next_refresh:acct1-u2", "ext_seen_at:browser1", "other:acct1-u2"} {
		_ = kvSet(k, "1")
	}
	purgeLegacySlotProfiles()
	if accountByID(ghost) != nil {
		t.Fatal("legacy -uN account not removed")
	}
	if accountByID(keep) == nil {
		t.Fatal("real account removed")
	}
	if kvGet("ext_seen_at:browser1-u3") != "" || kvGet("browser_next_refresh:acct1-u2") != "" {
		t.Fatal("legacy kv not removed")
	}
	if kvGet("ext_seen_at:browser1") == "" || kvGet("other:acct1-u2") == "" {
		t.Fatal("unrelated kv removed")
	}
}
