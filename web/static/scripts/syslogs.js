window.SysLogsModule = (() => {
    let logTimerId = null;
    let isAutoScrollEnabled = true;

    async function fetchLogs() {
        try {
            const res = await fetch('/api/v1/system/logs?limit=150');
            if (!res.ok) return;
            const logs = await res.json();
            renderLogs(logs);
        } catch (e) {
            console.error('Failed to sync system logs:', e);
        }
    }

    function renderLogs(logs) {
        const container = document.getElementById('syslog-entries-container');
        if (!container || !Array.isArray(logs)) return;

        container.innerHTML = logs.map(log => {
            let dateStr = '';
            if (log.timestamp) {
                const ms = parseInt(log.timestamp, 10) / 1000;
                if (!isNaN(ms)) {
                    dateStr = new Date(ms).toLocaleTimeString();
                }
            }

            return `
            <div class="syslog-row priority-${log.priority || '6'}">
            <span class="syslog-time">${dateStr}</span>
            <span class="syslog-ident">[${escapeHtml(log.identifier || 'kernel')}]</span>
            <span class="syslog-msg">${escapeHtml(log.message)}</span>
            </div>
            `;
        }).join('');

        if (isAutoScrollEnabled) {
            container.scrollTop = container.scrollHeight;
        }
    }

    function escapeHtml(str) {
        if (!str) return '';
        return String(str)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&quot;")
        .replace(/'/g, "&#039;");
    }

    function handleScroll(e) {
        const container = e.target;
        const atBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 40;
        isAutoScrollEnabled = atBottom;
    }

    return {
        init: () => {
            fetchLogs();
            logTimerId = setInterval(fetchLogs, 5000); // Poll every 5s

            const container = document.getElementById('syslog-entries-container');
            if (container) {
                container.addEventListener('scroll', handleScroll);
            }

            // Return cleanup teardown method for app.js router
            return () => {
                if (logTimerId) clearInterval(logTimerId);
                if (container) {
                    container.removeEventListener('scroll', handleScroll);
                }
            };
        }
    };
})();
