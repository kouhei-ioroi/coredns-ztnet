#!/bin/bash
# zt2hosts: Contact ZTNET API, and convert a list of authorized network members to hosts(5) format

set -eo pipefail

## -----------------------------------------------------------------------------

# Function to check if the zone name is valid
is_valid_zone() {
  if [[ $1 =~ ^([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,6}$ ]]; then
    return 0 
  else
    return 1
  fi
}

[[ -z "$ZTNET_API_TOKEN" ]] && \
  >&2 echo "ERROR: must set ZTNET_API_TOKEN!" && \
  exit 1

[ "$1" = "" ] && \
  >&2 echo "ERROR: must provide at least one network ID!" && \
  exit 1

## -----------------------------------------------------------------------------
API_ADDRESS=${API_ADDRESS:-"http://localhost:3000"}
API_URL="${API_ADDRESS}/api/v1"
AUTH_HEADER="x-ztnet-auth: ${ZTNET_API_TOKEN}"

## -----------------------------------------------------------------------------
get_network_info() { curl -sL -fH "${AUTH_HEADER}" "$API_URL/network/${1}"; }
get_network_members() { curl -sL -fH "${AUTH_HEADER}" "${API_URL}/network/${1}/member"; }

print_ipv6_id() {
  printf "%s:%s:%s" \
    $(echo "$1" | cut -c1-2) \
    $(echo "$1" | cut -c3-6) \
    $(echo "$1" | cut -c7-10)
}

print_rfc4193() {
  printf "fd%s:%s:%s:%s:%s99:93%s" \
    $(echo "$2" | cut -c1-2) \
    $(echo "$2" | cut -c3-6) \
    $(echo "$2" | cut -c7-10) \
    $(echo "$2" | cut -c11-14) \
    $(echo "$2" | cut -c15-16) \
    $(print_ipv6_id "$1")
}

print_6plane() {
  local TOP=${2:0:8}
  local BOT=${2:9:16}
  local hashed=$(printf '%x\n' "$(( 0x$TOP ^ 0x$BOT ))")

  printf "fc%s:%s:%s%s:0000:0000:0001" \
    $(echo "$hashed" | cut -c1-2) \
    $(echo "$hashed" | cut -c3-6) \
    $(echo "$hashed" | cut -c7-8) \
    $(print_ipv6_id "$1")
}

## -----------------------------------------------------------------------------
ipv4_lines=("127.0.0.1 localhost")
ipv6_lines=("::1 localhost ip6-localhost ip6-loopback")

for NETWORK in $@; do
  mapfile -td \: FIELDS < <(printf "%s\0" "$NETWORK")
  DNSNAME="${FIELDS[0]}"
  NETWORK="${FIELDS[1]}"

  # Check if the zone name is valid
  if [ -n "$DNSNAME" ] && ! is_valid_zone "$DNSNAME"; then
    >&2 echo "ERROR: Invalid domain name '$DNSNAME'"
    exit 1
  fi

  netmembers=$(get_network_members "$NETWORK")
  netinfo=$(get_network_info "$NETWORK")
  
  # check if "error" is in the response
  if [[ "$netinfo" == *"error"* ]]; then
    >&2 echo "ERROR GET Network: $netinfo"
    exit 1
  fi
  
  # check if "error" is in the response
  if [[ "$netmembers" == *"error"* ]]; then
    >&2 echo "ERROR GET Network Members: $netmembers"
    exit 1
  fi

joined=$(echo "$netmembers" | \
  jq '.[] | select(.authorized == true) | 
      { name: (.name | gsub(" "; "_")), id: .id, ips: .ipAssignments }')

  v6conf=$(echo "$netinfo" | jq -c '.v6AssignMode')
  sixplane=$(echo "$v6conf" | jq -r '.["6plane"]')
  rfc4193=$(echo "$v6conf" | jq -r '.rfc4193')

  for entry in $(echo "$joined" | jq -c '.'); do
    nodeid=$(echo "$entry" | jq -r '.id')
    nodename=$(echo "$entry" | jq -r '.name')

    for ipv4_address in $(echo "$entry" | jq -r '.ips[]'); do
      line=$(printf "%s\t%s\t%s" \
        "$ipv4_address" \
        "$nodename.$DNSNAME" \
        "$nodeid.$DNSNAME")

      ipv4_lines+=("$line")
    done
  done

  for entry in $(echo "$joined" | jq -c '.'); do
    nodeid=$(echo "$entry" | jq -r '.id')

    if [ "$rfc4193" = "true" ]; then
      line=$(printf "%s\t%s.%s\t%s" \
        $(print_rfc4193 "$nodeid" "$NETWORK") \
        $(echo "$entry" | jq -r '.name') \
        "$DNSNAME" \
        "$nodeid.$DNSNAME")
      ipv6_lines+=("$line")
    fi

    if [ "$sixplane" = "true" ]; then
      line=$(printf "%s\t%s.%s\t%s" \
        $(print_6plane "$nodeid" "$NETWORK") \
        $(echo "$entry" | jq -r '.name') \
        "$DNSNAME" \
        "$nodeid.$DNSNAME")
      ipv6_lines+=("$line")
    fi
  done
done

## -----------------------------------------------------------------------------

(
  for x in "${ipv4_lines[@]}"; do printf "%s\n" "$x"; done
  for x in "${ipv6_lines[@]}"; do printf "%s\n" "$x"; done
) | column -t