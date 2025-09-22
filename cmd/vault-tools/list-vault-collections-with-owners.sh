#!/bin/bash

# Script to list all Vault selfservice collections with their current owners
# and generate a YAML template for assigning ROVER groups for GSM migration
#
# Output format:
# vault_collections:
#   collection1:
#     owners:
#       - person1
#       - person2
#     rover-groups: []
#
# collections_without_groups:
#   - collection-with-no-vault-group

set -euo pipefail

export VAULT_ADDR="${VAULT_ADDR:-https://vault.ci.openshift.org}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $*" >&2
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

# Show progress bar
# Usage: show_progress <current> <total>
show_progress() {
    local current=$1
    local total=$2
    local percent=$((current * 100 / total))
    local completed=$((percent / 5))  # Scale to 20 chars
    local remaining=$((20 - completed))

    printf "\r${GREEN}[INFO]${NC} Progress: [" >&2
    printf "%${completed}s" | tr ' ' '=' >&2
    printf "%${remaining}s" | tr ' ' ' ' >&2
    printf "] %3d%% (%d/%d)   " "$percent" "$current" "$total" >&2
}

# Verify vault authentication
verify_vault_auth() {
    if ! vault token lookup &>/dev/null; then
        log_error "Vault authentication failed. Please run: vault login -method=oidc"
        exit 1
    fi
    log_info "Vault authentication verified"
}

# Get all top-level collections (folders) in kv/selfservice
get_collections() {
    local collections
    collections=$(vault kv list -format=json kv/selfservice 2>/dev/null | jq -r '.[] | select(endswith("/"))' | sed 's|/$||')

    if [ -z "$collections" ]; then
        log_error "No collections found in kv/selfservice"
        exit 1
    fi

    echo "$collections"
}

# Get members for a specific Vault group
# Returns a JSON array of member names, or empty array if group doesn't exist
get_group_members() {
    local collection_name="$1"
    local group_name="secret-collection-manager-managed-${collection_name}"

    # Try to read the group
    local group_data
    if ! group_data=$(vault read -format=json "identity/group/name/${group_name}" 2>/dev/null); then
        # Group doesn't exist
        echo "[]"
        return 0
    fi

    # Extract member entity IDs
    local member_ids
    member_ids=$(echo "$group_data" | jq -r '.data.member_entity_ids // []')

    if [ "$(echo "$member_ids" | jq 'length')" -eq 0 ]; then
        # No members in group
        echo "[]"
        return 0
    fi

    # Resolve each entity ID to their alias name
    local members=()
    while IFS= read -r entity_id; do
        if [ -z "$entity_id" ] || [ "$entity_id" = "null" ]; then
            continue
        fi

        local entity_data
        if entity_data=$(vault read -format=json "identity/entity/id/${entity_id}" 2>/dev/null); then
            # Extract the first alias name (assuming one alias per entity)
            local alias_name
            alias_name=$(echo "$entity_data" | jq -r '.data.aliases[0].name // empty')

            if [ -n "$alias_name" ]; then
                members+=("$alias_name")
            fi
        fi
    done < <(echo "$member_ids" | jq -r '.[]')

    # Convert bash array to JSON array (use -c for compact/single-line output)
    # Handle empty array explicitly to ensure we always return []
    if [ ${#members[@]} -eq 0 ]; then
        echo "[]"
    else
        printf '%s\n' "${members[@]}" | jq -Rc . | jq -sc .
    fi
}

# List owners for a specific collection
list_collection_owners() {
    local collection="$1"

    log_info "Querying owners for collection: ${collection}"
    verify_vault_auth

    local members_json
    members_json=$(get_group_members "$collection")

    local member_count
    member_count=$(echo "$members_json" | jq 'length')

    if [ "$member_count" -eq 0 ]; then
        local group_name="secret-collection-manager-managed-${collection}"
        if vault read "identity/group/name/${group_name}" &>/dev/null; then
            log_warn "Collection '${collection}' has a group but no members"
            echo "Owners: (none)"
        else
            log_warn "Collection '${collection}' has no Vault group (not managed by vault-secret-collection-manager)"
            echo "No Vault group found for collection: ${collection}"
            return 1
        fi
    else
        log_info "Found ${member_count} owner(s)"
        echo "Owners for collection '${collection}':"
        echo "$members_json" | jq -r '.[]' | while read -r member; do
            echo "  - ${member}"
        done
    fi
}

# Main function
main() {
    local output_file="${1:-vault-collections-owners.yaml}"

    log_info "Starting Vault collections scan..."
    log_info "Output file: ${output_file}"

    verify_vault_auth

    log_info "Fetching collections from kv/selfservice..."
    local collections
    collections=$(get_collections)

    local collection_count
    collection_count=$(echo "$collections" | wc -l | xargs)
    log_info "Found ${collection_count} collections"

    # Create temporary file for building YAML
    local temp_file
    temp_file=$(mktemp)
    trap "rm -f ${temp_file}" EXIT

    # Arrays to track collections with and without groups
    declare -a collections_with_groups
    declare -a collections_without_groups

    # Progress tracking
    local current=0
    local total=$collection_count

    # Process each collection
    while IFS= read -r collection; do
        ((current++)) || true
        if [ -z "$collection" ]; then
            continue
        fi

        show_progress "$current" "$total"

        local members_json
        members_json=$(get_group_members "$collection")

        # Validate JSON and default to empty array if invalid
        if ! echo "$members_json" | jq empty 2>/dev/null; then
            members_json="[]"
        fi

        local member_count
        member_count=$(echo "$members_json" | jq 'length' 2>/dev/null || echo "0")

        if [ "$member_count" -eq 0 ]; then
            # Check if group exists at all
            local group_name="secret-collection-manager-managed-${collection}"
            if vault read "identity/group/name/${group_name}" &>/dev/null; then
                collections_with_groups+=("$collection")
                echo "$collection|$members_json" >> "$temp_file"
            else
                collections_without_groups+=("$collection")
            fi
        else
            collections_with_groups+=("$collection")
            echo "$collection|$members_json" >> "$temp_file"
        fi
    done <<< "$collections"

    # Clear the progress bar line
    echo "" >&2

    # Generate YAML output
    log_info "Generating YAML output..."

    {
        echo "# Vault Self-Service Collections with Current Owners"
        echo "# Generated on: $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
        echo "#"
        echo "# Instructions:"
        echo "# 1. For each collection, assign ROVER group email(s) to 'rover-groups'"
        echo "# 2. ROVER groups will be granted access to the GSM secret collection"
        echo "# 3. Current 'owners' are listed for reference (Vault group members)"
        echo ""
        echo "vault_collections:"

        if [ ${#collections_with_groups[@]} -gt 0 ]; then
            # Sort collections alphabetically
            IFS=$'\n' sorted_collections=($(sort <<<"${collections_with_groups[*]}"))
            unset IFS

            for collection in "${sorted_collections[@]}"; do
                # Find the collection data in temp file
                local collection_data
                collection_data=$(grep "^${collection}|" "$temp_file" || echo "")

                if [ -z "$collection_data" ]; then
                    continue
                fi

                local members_json
                members_json=$(echo "$collection_data" | cut -d'|' -f2-)

                # Validate JSON before processing (silently default to empty if invalid)
                if ! echo "$members_json" | jq empty 2>/dev/null; then
                    members_json="[]"
                fi

                local member_count
                member_count=$(echo "$members_json" | jq 'length' 2>/dev/null || echo "0")

                echo "  ${collection}:"
                if [ "$member_count" -eq 0 ]; then
                    echo "    owners: []"
                else
                    echo "    owners:"
                    echo "$members_json" | jq -r '.[]' 2>/dev/null | while read -r member; do
                        if [ -n "$member" ]; then
                            echo "      - ${member}"
                        fi
                    done
                fi
                echo "    rover-groups: []"
            done
        else
            echo "  {}"
        fi

        echo ""
        echo "# Collections without Vault groups (not managed by vault-secret-collection-manager)"
        echo "# These may need special handling during migration"
        echo "collections_without_groups:"

        if [ ${#collections_without_groups[@]} -gt 0 ]; then
            # Sort collections alphabetically
            IFS=$'\n' sorted_no_groups=($(sort <<<"${collections_without_groups[*]}"))
            unset IFS

            for collection in "${sorted_no_groups[@]}"; do
                echo "  - ${collection}"
            done
        else
            echo "  []"
        fi
    } > "$output_file"

    # Validate YAML syntax
    log_info "Validating YAML syntax..."
    if command -v python3 &>/dev/null; then
        if ! python3 -c "import yaml; yaml.safe_load(open('${output_file}'))" 2>/dev/null; then
            log_error "Generated YAML is invalid! File: ${output_file}"
            exit 1
        fi
        log_info "✓ YAML syntax is valid"
    else
        log_warn "python3 not available, skipping YAML validation"
    fi

    # Validate that all collections are accounted for
    local total_processed=$((${#collections_with_groups[@]} + ${#collections_without_groups[@]}))

    log_info "YAML output written to: ${output_file}"
    log_info "Summary:"
    log_info "  - Collections with groups: ${#collections_with_groups[@]}"
    log_info "  - Collections without groups: ${#collections_without_groups[@]}"
    log_info "  - Total processed: ${total_processed}"
    log_info "  - Total found: ${collection_count}"

    # Critical validation: ensure no collections were dropped
    if [ "$total_processed" -ne "$collection_count" ]; then
        log_error "VALIDATION FAILED: Processed ${total_processed} but found ${collection_count} collections!"
        log_error "Some collections may have been dropped. Please review the script logic."
        exit 1
    fi

    log_info "✓ Validation passed: All ${collection_count} collections accounted for"
}

# Show usage
if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    echo "Usage: $0 [OPTIONS] [output-file]"
    echo ""
    echo "Lists all Vault selfservice collections with their current owners"
    echo "and generates a YAML template for assigning ROVER groups."
    echo ""
    echo "Options:"
    echo "  --collection NAME   Show owners for a specific collection only"
    echo "  -h, --help          Show this help message"
    echo ""
    echo "Arguments:"
    echo "  output-file         Path to output YAML file (default: vault-collections-owners.yaml)"
    echo "                      Ignored when --collection is used"
    echo ""
    echo "Examples:"
    echo "  $0                          # Scan all collections, output to vault-collections-owners.yaml"
    echo "  $0 my-output.yaml           # Scan all collections, output to my-output.yaml"
    echo "  $0 --collection abc1        # Show owners for collection 'abc1' only"
    echo ""
    echo "Requirements:"
    echo "  - vault CLI must be installed and in PATH"
    echo "  - jq must be installed"
    echo "  - Must be authenticated to Vault (run: vault login -method=oidc)"
    echo "  - VAULT_ADDR defaults to https://vault.ci.openshift.org"
    exit 0
fi

# Check dependencies
for cmd in vault jq; do
    if ! command -v "$cmd" &>/dev/null; then
        log_error "Required command '$cmd' not found in PATH"
        exit 1
    fi
done

# Parse arguments
COLLECTION_MODE=false
COLLECTION_NAME=""
OUTPUT_FILE=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --collection)
            COLLECTION_MODE=true
            COLLECTION_NAME="$2"
            shift 2
            ;;
        --collection=*)
            COLLECTION_MODE=true
            COLLECTION_NAME="${1#*=}"
            shift
            ;;
        -*)
            log_error "Unknown option: $1"
            echo "Run '$0 --help' for usage information"
            exit 1
            ;;
        *)
            OUTPUT_FILE="$1"
            shift
            ;;
    esac
done

# Run in appropriate mode
if [ "$COLLECTION_MODE" = true ]; then
    if [ -z "$COLLECTION_NAME" ]; then
        log_error "--collection requires a collection name"
        exit 1
    fi
    list_collection_owners "$COLLECTION_NAME"
else
    main "${OUTPUT_FILE:-vault-collections-owners.yaml}"
fi