#!/bin/bash
set -e

# Grant socket binding capabilities directly to the binary
setcap cap_net_bind_service,cap_net_admin,cap_net_raw=ep /usr/local/bin/peakroute

FLAG_FILE="/etc/peakroute/.firstboot_done"

# Double-check flag in case script is executed manually
if [ -f "$FLAG_FILE" ]; then
    echo "[!] First-boot process has already run. Exiting."
    exit 0
fi

# Helper function to validate dotted-decimal netmasks
is_valid_netmask() {
    case "$1" in
        255.0.0.0|255.128.0.0|255.192.0.0|255.224.0.0|255.240.0.0|255.248.0.0|255.252.0.0|255.254.0.0|\
        255.255.0.0|255.255.128.0|255.255.192.0|255.255.224.0|255.255.240.0|255.255.248.0|255.255.252.0|255.255.254.0|\
        255.255.255.0|255.255.255.128|255.255.255.192|255.255.255.224|255.255.255.240|255.255.255.248|255.255.255.252)
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

echo "[+] Running PeakRoute OS First-Time Setup..."
echo ""
echo "========================================================================="
echo "PeakRoute OS - Welcome"
echo "========================================================================="

# Extract physical network interfaces (Name Status MAC) as whole lines
mapfile -t INTERFACES < <(ip -br link show | awk '$1 ~ /^(eth|en|wl)/ {print $1, $2, $3}')

if [ ${#INTERFACES[@]} -lt 1 ]; then
    echo "[-] Error: No physical network interfaces detected!" >&2
    exit 1
fi

# -------------------------------------------------------------------------
# Step 1: Select Operation Mode
# -------------------------------------------------------------------------
echo ""
echo "Select PeakRoute OS Operation Mode:"
echo "  [0] Router Mode (NAT Gateway, Firewall, DHCP/DNS)"
echo "  [1] Access Point Mode (L2 Bridge, Upstream DHCP)"
read -p "Select choice [0-1]: " MODE_CHOICE < /dev/tty

case "$MODE_CHOICE" in
    0) OPER_MODE="router" ;;
    1) OPER_MODE="ap" ;;
    *) echo "[!] Invalid selection. Defaulting to Router Mode."; OPER_MODE="router" ;;
esac

echo "[+] Selected Mode: ${OPER_MODE^^}"

# -------------------------------------------------------------------------
# Step 2: Interface & IP Assignment Logic
# -------------------------------------------------------------------------
if [ "$OPER_MODE" = "router" ]; then
    if [ ${#INTERFACES[@]} -lt 2 ]; then
        echo "[-] Error: Router Mode requires at least 2 physical network interfaces." >&2
        exit 1
    fi

    # --- Select WAN ---
    while true; do
        echo ""
        echo "Select your WAN (Internet) Interface:"
        for i in "${!INTERFACES[@]}"; do
            echo "  [$i] ${INTERFACES[$i]}"
        done
        read -p "Select choice [0-$((${#INTERFACES[@]} - 1))]: " choice < /dev/tty

        if [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 0 ] && [ "$choice" -lt "${#INTERFACES[@]}" ]; then
            WAN_IFACE=$(echo "${INTERFACES[$choice]}" | awk '{print $1}')
            break
        fi
    done

    # Filter out WAN for LAN candidates
    LAN_CANDIDATES=()
    for line in "${INTERFACES[@]}"; do
        iface=$(echo "$line" | awk '{print $1}')
        if [ "$iface" != "$WAN_IFACE" ]; then
            LAN_CANDIDATES+=("$line")
        fi
    done

    # --- Select LAN(s) ---
    while true; do
        echo ""
        echo "Select LAN Interface(s) (space-separated choices, e.g., '0 1'):"
        for i in "${!LAN_CANDIDATES[@]}"; do
            echo "  [$i] ${LAN_CANDIDATES[$i]}"
        done
        read -p "Select choice(s): " -a choices < /dev/tty

        SELECTED_LAN_IFACES=()
        for idx in "${choices[@]}"; do
            if [[ "$idx" =~ ^[0-9]+$ ]] && [ "$idx" -ge 0 ] && [ "$idx" -lt "${#LAN_CANDIDATES[@]}" ]; then
                iface=$(echo "${LAN_CANDIDATES[$idx]}" | awk '{print $1}')
                SELECTED_LAN_IFACES+=("$iface")
            fi
        done

        if [ ${#SELECTED_LAN_IFACES[@]} -gt 0 ]; then
            break
        fi
    done

    # --- Prompt 1: LAN Gateway IP Address ---
    while true; do
        echo ""
        read -p "Enter LAN Gateway IP Address [default: 192.168.1.1]: " LAN_IP < /dev/tty
        LAN_IP=${LAN_IP:-"192.168.1.1"}

        if [[ $LAN_IP =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            break
        else
            echo "[-] Invalid IPv4 address format. Example: 192.168.1.1"
        fi
    done

    # --- Prompt 2: Subnet Mask ---
    while true; do
        read -p "Enter Subnet Mask [default: 255.255.255.0]: " NETMASK < /dev/tty
        NETMASK=${NETMASK:-"255.255.255.0"}

        if is_valid_netmask "$NETMASK"; then
            break
        else
            echo "[-] Invalid subnet mask. Common choices: 255.255.255.0, 255.255.0.0"
        fi
    done

    # Derive default base octets for DHCP defaults
    SUBNET_PREFIX=$(echo "$LAN_IP" | cut -d'.' -f1-3)

    # --- Prompt 3: DHCP Server Range ---
    echo ""
    echo "Configure DHCP Server Range for LAN clients:"
    read -p "  Start IP [default: ${SUBNET_PREFIX}.100]: " DHCP_START < /dev/tty
    DHCP_START=${DHCP_START:-"${SUBNET_PREFIX}.100"}

    read -p "  End IP   [default: ${SUBNET_PREFIX}.200]: " DHCP_END < /dev/tty
    DHCP_END=${DHCP_END:-"${SUBNET_PREFIX}.200"}

    # --- Configure LAN Interface / Bridge ---
    EXTRA_BRIDGE_STANZA=""
    if [ ${#SELECTED_LAN_IFACES[@]} -gt 1 ]; then
        LAN_TARGET="br0"
        EXTRA_BRIDGE_STANZA="    bridge_ports ${SELECTED_LAN_IFACES[*]}"
        echo "[+] Multiple LAN interfaces selected. Creating bridge 'br0'..."
        ip link add name br0 type bridge || true
        for iface in "${SELECTED_LAN_IFACES[@]}"; do
            ip link set dev "$iface" master br0
            ip link set dev "$iface" up
        done
    else
        LAN_TARGET="${SELECTED_LAN_IFACES[0]}"
    fi

    # Bring up LAN target interface
    ip link set dev "$LAN_TARGET" up

    # Bring up WAN interface with DHCP client
    dhclient "$WAN_IFACE" 2>/dev/null || true

    # --- Configure Persistent Network Interfaces (/etc/network/interfaces) ---
    cat << EOF > /etc/network/interfaces
auto lo
iface lo inet loopback

auto $WAN_IFACE
iface $WAN_IFACE inet dhcp

auto $LAN_TARGET
iface $LAN_TARGET inet static
    address $LAN_IP
    netmask $NETMASK
$EXTRA_BRIDGE_STANZA
EOF

    # --- Configure dnsmasq for LAN DHCP & DNS ---
    cat << EOF > /etc/dnsmasq.d/peakroute-dhcp.conf
interface=$LAN_TARGET
dhcp-range=$DHCP_START,$DHCP_END,$NETMASK,12h
dhcp-option=option:router,$LAN_IP
dhcp-option=option:dns-server,$LAN_IP
EOF

    # Restart services cleanly
    systemctl restart networking.service 2>/dev/null || true
    systemctl restart dnsmasq 2>/dev/null || true

    # --- Generate Router nftables ---
    NFT_LAN_TARGET="\"$LAN_TARGET\""

    cat << EOF > /etc/nftables.conf
#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept
        ct state invalid drop
        iifname $NFT_LAN_TARGET accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iifname $NFT_LAN_TARGET oifname "$WAN_IFACE" accept
    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}

table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "$WAN_IFACE" masquerade
    }
}
EOF

    echo "net.ipv4.ip_forward=1" > /etc/sysctl.d/99-peakroute-forwarding.conf
    sysctl -p /etc/sysctl.d/99-peakroute-forwarding.conf > /dev/null
    systemctl enable --now nftables

elif [ "$OPER_MODE" = "ap" ]; then
    echo ""
    echo "Select Interface(s) to add to the Access Point Bridge (br0):"
    for i in "${!INTERFACES[@]}"; do
        echo "  [$i] ${INTERFACES[$i]}"
    done
    read -p "Select choice(s) (e.g. '0 1'): " -a choices < /dev/tty

    AP_IFACES=()
    for idx in "${choices[@]}"; do
        if [[ "$idx" =~ ^[0-9]+$ ]] && [ "$idx" -ge 0 ] && [ "$idx" -lt "${#INTERFACES[@]}" ]; then
            iface=$(echo "${INTERFACES[$idx]}" | awk '{print $1}')
            AP_IFACES+=("$iface")
        fi
    done

    # --- Create Linux Bridge (br0) ---
    echo "[+] Setting up bridge 'br0' for AP Mode..."
    ip link add name br0 type bridge || true
    ip link set dev br0 up

    for iface in "${AP_IFACES[@]}"; do
        ip link set dev "$iface" master br0
        ip link set dev "$iface" up
    done

    # Persistent bridge config via DHCP from upstream
    cat << EOF > /etc/network/interfaces
auto lo
iface lo inet loopback

auto br0
iface br0 inet dhcp
    bridge_ports ${AP_IFACES[*]}
EOF

    dhclient br0 2>/dev/null || true
    systemctl restart networking.service 2>/dev/null || true

    # Pass-through firewall
    cat << 'EOF' > /etc/nftables.conf
#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input { type filter hook input priority filter; policy accept; }
    chain forward { type filter hook forward priority filter; policy accept; }
    chain output { type filter hook output priority filter; policy accept; }
}
EOF

    # Disable standalone DHCP server in AP mode
    systemctl disable --now dnsmasq 2>/dev/null || true
    systemctl enable --now nftables
fi

# -------------------------------------------------------------------------
# Step 3: Complete First Boot
# -------------------------------------------------------------------------
mkdir -p /etc/peakroute
touch "$FLAG_FILE"

# Read system hostname from /etc/hostname (or fallback to hostname command)
if [ -f "/etc/hostname" ]; then
    CURRENT_HOSTNAME=$(cat /etc/hostname | tr -d '\r\n')
else
    CURRENT_HOSTNAME=$(hostname)
fi

# Write or update /etc/peakroute/sys.json
cat << EOF > /etc/peakroute/sys.json
{
  "hostname": "$HOSTNAME",
  "operation_mode": "$OPER_MODE",
  "configured": true
}

EOF
# Format SELECTED_LAN_IFACES array for JSON (e.g. ["eth1", "eth2"])
LAN_JSON_ARRAY=$(printf '"%s",' "${SELECTED_LAN_IFACES[@]}")
LAN_JSON_ARRAY="[${LAN_JSON_ARRAY%,}]" # remove trailing comma

cat << EOF > /etc/peakroute/network.json
{
    "wan_interface": "$WAN_IFACE",
    "wan_mode": "dhcp",
    "wan_ip": "",
    "wan_subnet": "",
    "wan_gateway": "",
    "wan_dns": [],
    "lan_interfaces": $LAN_JSON_ARRAY,
    "lan_ip": "$LAN_IP",
    "lan_subnet": "$NETMASK",
    "interfaces": [],
    "managed_switches": []
}
EOF


# Clean up temporary first-boot autologin
rm -rf /etc/systemd/system/getty@tty1.service.d/
systemctl daemon-reload

# Restart peakroute daemon to pick up the new network config
systemctl restart peakroute.service

echo "=================================================="
echo "[+] First-boot setup completed successfully in ${OPER_MODE^^} mode!"
echo "Please press ENTER to continue..."
echo "=================================================="

read -r
