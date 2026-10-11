// web/static/scripts/dhcp.js

window.DHCPModule = (function () {
    let pollInterval = null;

    // How the device is attached, as an icon: Wi-Fi, or an Ethernet plug. The details (network and band) are in
    // the tooltip and the accessible name; a dash means the Wi-Fi side could not be read, so a cable is not assumed.
    const ICON_WIFI = '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 8.8a15 15 0 0 1 20 0"/><path d="M5.5 12.4a10 10 0 0 1 13 0"/><path d="M9 16a5 5 0 0 1 6 0"/><circle cx="12" cy="19.5" r="0.9" fill="currentColor"/></svg>';
    const ICON_ETHERNET = '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 3h12v9a3 3 0 0 1-3 3H9a3 3 0 0 1-3-3z"/><path d="M10 15v3h4v-3"/><path d="M12 18v3"/><path d="M9.5 3v4M12 3v4M14.5 3v4"/></svg>';

    function connectionHtml(c) {
        if (c.connection === 'wifi') {
            const label = ['Wi-Fi', c.ssid, c.band].filter(Boolean).join(' \u00b7 ');
            return `<span class="conn-icon conn-icon--wifi" role="img" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}">${ICON_WIFI}</span>`;
        }
        if (c.connection === 'wired') {
            return `<span class="conn-icon" role="img" aria-label="Wired" title="Wired">${ICON_ETHERNET}</span>`;
        }
        return '<span class="form-hint" title="The Wi-Fi side could not be read" aria-label="Unknown">\u2013</span>';
    }

    async function fetchAllLeases() {
        const res = await apiFetch('/api/v1/dhcp/leases');
        if (!res || !res.ok) return;

        const leases = await res.json();
        const tbody = document.getElementById('all-clients-table-body');
        const badge = document.getElementById('lease-count-badge');

        if (badge) {
            badge.textContent = `${leases.length} Active ${leases.length === 1 ? 'Lease' : 'Leases'}`;
        }

        if (!tbody) return;

        if (!leases || leases.length === 0) {
            tbody.innerHTML = `<tr><td colspan="6" class="table-placeholder">No active DHCP leases found.</td></tr>`;
            return;
        }

        tbody.innerHTML = leases.map(c => `
        <tr>
        <td><strong>${escapeHtml(c.hostname)}</strong></td>
        <td>${escapeHtml(c.ip_address)}</td>
        <td><code>${escapeHtml(c.mac_address)}</code></td>
        <td>${connectionHtml(c)}</td>
        <td>${escapeHtml(c.expiry_text)}</td>
        <td>${formatBytes(c.total_bytes || 0)}</td>
        </tr>
        `).join('');
    }

    function init() {
        fetchAllLeases();
        // Polls full lease table every 3 seconds while active
        pollInterval = setInterval(fetchAllLeases, 3000);

        // Return cleanup hook called by app.js when leaving the view
        return function destroy() {
            if (pollInterval) {
                clearInterval(pollInterval);
                pollInterval = null;
            }
        };
    }

    return { init };
})();
