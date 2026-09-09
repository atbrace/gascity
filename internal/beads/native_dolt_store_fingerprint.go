package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// errNativeFingerprintUnavailable is returned when the native storage handle
// does not expose its raw database. Callers treat it as "assume changed", the
// same as any other fingerprint error; it is never a reason to reconnect.
var errNativeFingerprintUnavailable = errors.New("native active fingerprint: storage exposes no raw DB")

// ActiveFingerprint implements ActiveFingerprinter for the native store. It is
// the same activeFingerprintSQL aggregate BdStore forks `bd sql` for, run
// in-process over the raw *sql.DB the storage handle already exposes (the
// escape hatch repairIDDefault uses). Without it a native-backed control-ready
// cache would lose the cheap "nothing changed" probe and re-prime the whole
// open surface on every TTL lapse.
func (s *NativeDoltStore) ActiveFingerprint() (string, error) {
	var fingerprint string
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		accessor, ok := storage.(rawDBGetter)
		if !ok || accessor.DB() == nil {
			return errNativeFingerprintUnavailable
		}
		fp, err := activeFingerprintFromDB(ctx, accessor.DB())
		if err != nil {
			return fmt.Errorf("native active fingerprint: %w", err)
		}
		fingerprint = fp
		return nil
	})
	return fingerprint, err
}

// activeFingerprintFromDB runs the aggregate and joins the three columns in
// activeFingerprintColumns order with '|', as raw bytes: the token is only
// ever compared for equality against an earlier token from the same store.
func activeFingerprintFromDB(ctx context.Context, db *sql.DB) (string, error) {
	var n, mx, nb []byte
	if err := db.QueryRowContext(ctx, activeFingerprintSQL()).Scan(&n, &mx, &nb); err != nil {
		return "", err
	}
	return string(n) + "|" + string(mx) + "|" + string(nb) + "|", nil
}
