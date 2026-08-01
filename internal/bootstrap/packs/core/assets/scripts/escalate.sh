#!/usr/bin/env bash
# escalate — generic Core escalation hook for deterministic maintenance scripts.
#
# Packs can override escalation by shipping assets/scripts/escalate.sh and
# placing that pack earlier in GC_ESCALATE_SEARCH_PACKS.
#
# Severity picks the destination:
#
#   CRITICAL, HIGH  -> page tier. A person should see an outage, so these go
#                      to `human` (GC_ESCALATION_PAGE_RECIPIENT to redirect).
#   anything else   -> triage tier. Advisories are not pages; they go to
#                      GC_ESCALATION_TRIAGE_RECIPIENT, and are reported
#                      without mailing when no triage mailbox is configured.
#
# The triage tier ships no default recipient because `human` is the only
# recipient every city is guaranteed to resolve — anything else is a
# user-configured agent name, and Core does not hardcode roles.
set -euo pipefail

SUBJECT=""
MESSAGE=""
SEVERITY=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        --subject)
            [ "$#" -ge 2 ] || { echo "escalate: --subject requires a value" >&2; exit 2; }
            SUBJECT="$2"
            shift 2
            ;;
        --message|-m)
            [ "$#" -ge 2 ] || { echo "escalate: --message requires a value" >&2; exit 2; }
            MESSAGE="$2"
            shift 2
            ;;
        --severity)
            [ "$#" -ge 2 ] || { echo "escalate: --severity requires a value" >&2; exit 2; }
            SEVERITY="$2"
            shift 2
            ;;
        --)
            shift
            break
            ;;
        *)
            echo "escalate: unknown argument $1" >&2
            exit 2
            ;;
    esac
done

if [ -z "$SUBJECT" ]; then
    echo "escalate: --subject is required" >&2
    exit 2
fi

if [ -n "$SEVERITY" ] && ! printf '%s' "$SUBJECT" | grep -Eq '\[[^]]+\]$'; then
    SUBJECT="$SUBJECT [$SEVERITY]"
fi

case "$(printf '%s' "$SEVERITY" | tr '[:lower:]' '[:upper:]')" in
    CRITICAL|HIGH)
        TIER_RECIPIENT="${GC_ESCALATION_PAGE_RECIPIENT:-human}"
        ;;
    *)
        TIER_RECIPIENT="${GC_ESCALATION_TRIAGE_RECIPIENT:-}"
        ;;
esac

RECIPIENT="${GC_ESCALATION_RECIPIENT:-$TIER_RECIPIENT}"

if [ -z "$RECIPIENT" ]; then
    printf 'escalate: no triage mailbox configured; not mailing %s escalation.\n' \
        "${SEVERITY:-unspecified-severity}"
    printf 'escalate: set GC_ESCALATION_TRIAGE_RECIPIENT to route it to an agent mailbox.\n'
    printf 'escalate: subject: %s\n' "$SUBJECT"
    printf 'escalate: message: %s\n' "$MESSAGE"
    exit 0
fi

gc mail send "$RECIPIENT" -s "$SUBJECT" -m "$MESSAGE"
