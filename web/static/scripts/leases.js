// web/static/scripts/dhcp.js

window.DHCPModule = (function () {
    let pollInterval = null;

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
            tbody.innerHTML = `<tr><td colspan="5" class="table-placeholder">No active DHCP leases found.</td></tr>`;
            return;
        }

        tbody.innerHTML = leases.map(c => `
        <tr>
        <td><strong>${escapeHtml(c.hostname)}</strong></td>
        <td>${escapeHtml(c.ip_address)}</td>
        <td><code>${escapeHtml(c.mac_address)}</code></td>
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
