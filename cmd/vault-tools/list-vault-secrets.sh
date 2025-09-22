#!/bin/bash

# Script to list all secrets in kv/selfservice subfolders and their key names
# Output: JSON format with full paths for easy parsing

export VAULT_ADDR='https://vault.ci.openshift.org'

# Function to recursively list all secret paths
list_secrets() {
    local path="$1"

    # List items in current path
    local items=$(vault kv list -format=json "$path" 2>/dev/null)

    if [ $? -ne 0 ]; then
        # This might be a secret itself, not a folder
        echo "$path"
        return
    fi

    # Parse the JSON array
    echo "$items" | jq -r '.[]' | while read -r item; do
        if [[ "$item" == */ ]]; then
            # It's a folder, recurse into it
            list_secrets "${path}/${item%/}"
        else
            # It's a secret
            echo "${path}/${item}"
        fi
    done
}

# Temporary file to collect results
tmpfile=$(mktemp)

# Get all subfolders in selfservice (exclude direct secrets)
vault kv list -format=json kv/selfservice 2>/dev/null | jq -r '.[] | select(endswith("/"))' | while read -r folder; do
    folder_name="${folder%/}"

    # Get all secrets in this subfolder (recursively)
    list_secrets "kv/selfservice/$folder_name" | while read -r secret_path; do
        if [ -z "$secret_path" ]; then
            continue
        fi

        # Get only the keys (not the values)
        keys=$(vault kv get -format=json "$secret_path" 2>/dev/null | jq -c '.data.data | keys' 2>/dev/null)

        if [ $? -eq 0 ] && [ -n "$keys" ]; then
            # Output in JSON format
            echo "{\"path\": \"$secret_path\", \"keys\": $keys}" >> "$tmpfile"
        fi
    done
done

# Format the final JSON output
echo "{"
echo "  \"secrets\": ["
first=true
while IFS= read -r line; do
    if [ "$first" = true ]; then
        echo -n "    $line"
        first=false
    else
        echo ","
        echo -n "    $line"
    fi
done < "$tmpfile"
echo ""
echo "  ]"
echo "}"

# Cleanup
rm -f "$tmpfile"
