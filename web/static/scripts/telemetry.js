// web/static/scripts/telemetry.js

window.TelemetryModule = (function () {
    let pollInterval = null;

    // Safe fetch wrapper falling back to window.fetch if apiFetch is undefined
    async function safeFetch(url) {
        if (typeof apiFetch === 'function') {
            return await apiFetch(url);
        }
        try {
            return await fetch(url);
        } catch (e) {
            console.error(`Fetch error for ${url}:`, e);
            return null;
        }
    }

    // Helper: Byte formatter
    function formatBytes(bytes) {
        if (!bytes || bytes <= 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
    }

    // Helper: Uptime formatter
    function formatUptime(seconds) {
        if (!seconds || seconds <= 0) return '0s';
        const days = Math.floor(seconds / 86400);
        const hours = Math.floor((seconds % 86400) / 3600);
        const mins = Math.floor((seconds % 3600) / 60);
        const secs = Math.floor(seconds % 60);

        let parts = [];
        if (days > 0) parts.push(`${days}d`);
        if (hours > 0) parts.push(`${hours}h`);
        if (mins > 0) parts.push(`${mins}m`);
        if (secs > 0 || parts.length === 0) parts.push(`${secs}s`);

        return parts.join(' ');
    }

    async function fetchStats() {
        const res = await safeFetch('/api/v1/system/stats');
        if (!res || !res.ok) return;

        const data = await res.json();

        // 1. Hostname
        const hostnameEl = document.getElementById('hostname-val');
        if (hostnameEl && data.hostname) {
            hostnameEl.innerText = data.hostname;
        }

        // 2. Build Metadata, Arch & Kernel (Flat Structure)
        const version = data.peakroute_build || '--';
        let arch = data.arch || '';
        let rawKernel = data.kernel || data.kernel_version || '';

        // If arch is missing, extract embedded arch from kernel like "(amd64)"
        if (!arch && rawKernel) {
            const archMatch = rawKernel.match(/\(([^)]+)\)/);
if (archMatch) {
    arch = archMatch[1];
}
        }

        // Clean redundant parentheses from kernel string
        if (rawKernel) {
            rawKernel = rawKernel.replace(/\s*\([^)]+\)/g, '').trim();
        }

        const versionEl = document.getElementById('app-version-val');
        if (versionEl) versionEl.innerText = version;

        const archEl = document.getElementById('arch-val');
        if (archEl) archEl.innerText = arch || '--';

        const kernelEl = document.getElementById('kernel-val');
        if (kernelEl) kernelEl.innerText = rawKernel || '--';

        const cpuModelEl = document.getElementById('cpu-model-val');
        if (cpuModelEl) {
            cpuModelEl.innerText = data.cpu_model || 'Unknown CPU';
        }

        // 3. CPU Usage & Temperature
        const cpuValEl = document.getElementById('cpu-val');
        const cpuBarEl = document.getElementById('cpu-bar');
        if (cpuValEl && cpuBarEl) {
            const cpuPct = (data.cpu_usage_pct || 0).toFixed(1);
            cpuValEl.innerText = `${cpuPct}%`;
            cpuBarEl.style.width = `${cpuPct}%`;
        }

        const cpuTempEl = document.getElementById('cpu-temp-val');
        const cpuTempBarEl = document.getElementById('cpu-temp-bar');
        if (typeof data.cpu_temp_celsius === 'number') {
            const temp = data.cpu_temp_celsius.toFixed(1);
            if (cpuTempEl) cpuTempEl.innerText = `${temp} °C`;
            if (cpuTempBarEl) {
                const tempPct = Math.min(100, Math.max(0, data.cpu_temp_celsius));
                cpuTempBarEl.style.width = `${tempPct}%`;
            }
        } else {
            // the machine has no sensor we can trust: say so rather than show a wrong number
            if (cpuTempEl) cpuTempEl.innerText = 'n/a';
            if (cpuTempBarEl) cpuTempBarEl.style.width = '0%';
        }

        // 4. Load Averages (1m, 5m, 15m)
        const loadAvgEl = document.getElementById('load-avg-val');
        if (loadAvgEl && Array.isArray(data.load_avg)) {
            loadAvgEl.innerText = data.load_avg.map(n => Number(n).toFixed(2)).join(', ');
        }

        // 5. Memory Usage
        const memValEl = document.getElementById('mem-val');
        const memBarEl = document.getElementById('mem-bar');
        if (memValEl && memBarEl) {
            const memTotal = data.memory_total_mb || 1;
            const memUsed = data.memory_used_mb || 0;
            const memPct = Math.min(100, Math.round((memUsed / memTotal) * 100));

            memValEl.innerText = `${memUsed} MB / ${memTotal} MB`;
            memBarEl.style.width = `${memPct}%`;
        }

        // 6. SWAP Usage
        const swapValEl = document.getElementById('swap-val');
        const swapBarEl = document.getElementById('swap-bar');
        if (swapValEl && swapBarEl) {
            const swapTotal = data.swap_total_mb || 0;
            const swapUsed = data.swap_used_mb || 0;
            const swapPct = swapTotal > 0 ? Math.min(100, Math.round((swapUsed / swapTotal) * 100)) : 0;

            swapValEl.innerText = swapTotal > 0 ? `${swapUsed} MB / ${swapTotal} MB` : 'Disabled';
            swapBarEl.style.width = `${swapPct}%`;
        }

        // 7. Disk Storage Usage
        const diskValEl = document.getElementById('disk-val');
        const diskBarEl = document.getElementById('disk-bar');
        if (diskValEl && diskBarEl && data.disk) {
            const diskUsedMB = data.disk.used_mb || 0;
            const diskTotalMB = data.disk.total_mb || 1;
            const diskPct = (data.disk.used_percent || 0).toFixed(1);

            const usedGB = (diskUsedMB / 1024).toFixed(1);
            const totalGB = (diskTotalMB / 1024).toFixed(1);

            diskValEl.innerText = `${usedGB} GB / ${totalGB} GB (${diskPct}%)`;
            diskBarEl.style.width = `${diskPct}%`;
        }

        // 8. Firewall State Table (Conntrack)
        const stateTableValEl = document.getElementById('state-table-val');
        const stateTableBarEl = document.getElementById('state-table-bar');
        if (stateTableValEl && data.state_table) {
            const current = data.state_table.current || 0;
            const max = data.state_table.max || 1;
            const statePct = Math.min(100, ((current / max) * 100)).toFixed(1);

            stateTableValEl.innerText = `${current.toLocaleString()} / ${max.toLocaleString()}`;
            if (stateTableBarEl) {
                stateTableBarEl.style.width = `${statePct}%`;
            }
        }

        // 10. System Uptime
        const uptimeValEl = document.getElementById('uptime-val');
        if (uptimeValEl) {
            uptimeValEl.innerText = formatUptime(data.uptime_seconds || 0);
        }

        if (window.OverviewCards) window.OverviewCards.onStats(data);
    }

    async function fetchTopClients() {
        const res = await safeFetch('/api/v1/dhcp/leases');
        const tbody = document.getElementById('top-clients-table-body');
        if (!tbody) return;

        if (!res || !res.ok) {
            tbody.innerHTML = `<tr><td colspan="4" class="table-placeholder">No active LAN clients detected</td></tr>`;
            return;
        }

        const leases = (await res.json()) || [];
        if (window.OverviewCards) window.OverviewCards.onLeases(leases);

        if (!Array.isArray(leases) || leases.length === 0) {
            tbody.innerHTML = `<tr><td colspan="4" class="table-placeholder">No active LAN clients detected</td></tr>`;
            return;
        }

        // Sort by usage descending and slice top 5
        const topClients = leases
        .sort((a, b) => (b.total_bytes || 0) - (a.total_bytes || 0))
        .slice(0, 5);

        tbody.innerHTML = topClients.map(c => `
        <tr>
        <td><strong>${escapeHtml(c.hostname || 'Unknown')}</strong></td>
        <td>${escapeHtml(c.ip_address || c.ip || '--')}</td>
        <td><code>${escapeHtml(c.mac_address || c.mac || '--')}</code></td>
        <td>${formatBytes(c.total_bytes || 0)}</td>
        </tr>
        `).join('');
    }

    async function updateAll() {
        await fetchStats();
        await fetchTopClients();
    }

    function init() {
        if (window.OverviewCards) window.OverviewCards.start();
        updateAll();
        pollInterval = setInterval(updateAll, 1000);

        return function destroy() {
            if (window.OverviewCards) window.OverviewCards.stop();
            if (pollInterval) {
                clearInterval(pollInterval);
                pollInterval = null;
            }
        };
    }

    return { init };
})();
