// web/static/scripts/telemetry.js

window.TelemetryModule = (function () {
    let pollInterval = null;

    async function fetchStats() {
        const res = await apiFetch('/api/v1/system/stats');
        if (!res || !res.ok) return;

        const data = await res.json();

        // 1. CPU Usage
        const cpuValEl = document.getElementById('cpu-val');
        const cpuBarEl = document.getElementById('cpu-bar');
        if (cpuValEl && cpuBarEl) {
            const cpuPct = (data.cpu_usage_pct || 0).toFixed(1);
            cpuValEl.innerText = `${cpuPct}%`;
            cpuBarEl.style.width = `${cpuPct}%`;
        }

        // 2. Memory Usage
        const memValEl = document.getElementById('mem-val');
        const memBarEl = document.getElementById('mem-bar');
        if (memValEl && memBarEl) {
            const memTotal = data.memory_total_mb || 1;
            const memUsed = data.memory_used_mb || 0;
            const memPct = Math.min(100, Math.round((memUsed / memTotal) * 100));

            memValEl.innerText = `${memUsed} MB / ${memTotal} MB`;
            memBarEl.style.width = `${memPct}%`;
        }

        // 3. System Uptime
        const uptimeValEl = document.getElementById('uptime-val');
        if (uptimeValEl) {
            uptimeValEl.innerText = formatUptime(data.uptime_seconds || 0);
        }

        // 4. Interfaces Table
        const tbody = document.getElementById('iface-table-body');
        if (tbody && data.interfaces && Object.keys(data.interfaces).length > 0) {
            let rowsHtml = '';
            for (const [iface, stats] of Object.entries(data.interfaces)) {
                rowsHtml += `
                <tr>
                <td class="iface-name">${iface}</td>
                <td>${formatBytes(stats.rx_bytes)} <span class="pkt-subtext">(${stats.rx_packets.toLocaleString()} pkts)</span></td>
                <td>${formatBytes(stats.tx_bytes)} <span class="pkt-subtext">(${stats.tx_packets.toLocaleString()} pkts)</span></td>
                </tr>
                `;
            }
            tbody.innerHTML = rowsHtml;
        }
    }

    async function fetchTopClients() {
        const res = await apiFetch('/api/v1/dhcp/leases');
        const tbody = document.getElementById('top-clients-table-body');
        if (!tbody) return;

        if (!res || !res.ok) {
            tbody.innerHTML = `<tr><td colspan="4" class="table-placeholder">No active LAN clients detected</td></tr>`;
            return;
        }

        const leases = (await res.json()) || [];

        if (leases.length === 0) {
            tbody.innerHTML = `<tr><td colspan="4" class="table-placeholder">No active LAN clients detected</td></tr>`;
            return;
        }

        // Sort by usage descending and slice top 5
        const topClients = leases
        .sort((a, b) => (b.total_bytes || 0) - (a.total_bytes || 0))
        .slice(0, 5);

        tbody.innerHTML = topClients.map(c => `
        <tr>
        <td><strong>${c.hostname}</strong></td>
        <td>${c.ip_address}</td>
        <td><code>${c.mac_address}</code></td>
        <td>${formatBytes(c.total_bytes || 0)}</td>
        </tr>
        `).join('');
    }

    async function updateAll() {
        await fetchStats();
        await fetchTopClients();
    }

    function init() {
        // Immediate initial poll
        updateAll();

        // Start 1-second polling loop for both stats and top clients
        pollInterval = setInterval(updateAll, 1000);

        // Return cleanup hook to stop polling when switching tabs
        return function destroy() {
            if (pollInterval) {
                clearInterval(pollInterval);
                pollInterval = null;
            }
        };
    }

    return { init };
})();
