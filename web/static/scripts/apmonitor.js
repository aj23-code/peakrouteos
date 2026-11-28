// web/static/scripts/apmonitor.js
// The access point's monitoring token: lets a router that watches this access point (Network, Network devices) read its status without signing in.
// The token opens that one read and nothing else; only its hash is kept here, so it is shown once, when it is made. docs/device-pages.md.

window.APMonitorModule = (function () {
    let state = null;     // { enabled } from GET /api/v1/ap/monitoring, or null (not an access point)
    let token = '';       // shown once, right after it is made
    let confirmRevoke = false;
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    function render() {
        const box = $('ap-monitor');
        if (!box) return;
        if (!state) { box.innerHTML = ''; return; }
        const lock = busy ? ' disabled' : '';
        box.innerHTML = `<section class="card">
            <div class="net-card-head"><h2>Monitoring</h2><span class="tag ${state.enabled ? 'tag-ok' : ''}">${state.enabled ? 'A token is issued' : 'Off'}</span></div>
            <p class="form-hint">A router running PeakRoute can watch this access point: its ports, Wi-Fi and the devices connected, so that a device's page there says which access point it is on. The router asks with a read-only token (it can read this device's status and nothing else). Make a token here, then add this access point on the router's <em>Network devices</em> page with its address and the token. Making a new token stops the old one.</p>
            ${token ? `<div class="form-message form-message--ok" role="status">The token, shown only now: <code id="ap-monitor-token">${escapeHtml(token)}</code></div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--primary" id="apm-make"${lock}>${state.enabled ? 'Make a new token' : 'Make a token'}</button>
                ${state.enabled ? `<button type="button" class="cfg-btn cfg-btn--danger" id="apm-revoke"${lock}>${confirmRevoke ? 'Click again to turn off' : 'Turn off'}</button>` : ''}
            </div></section>`;
    }

    async function load() {
        const res = await apiFetch('/api/v1/ap/monitoring');
        state = res && res.ok ? await res.json() : null;   // 409: this device is not an access point
        render();
    }

    async function act(method) {
        busy = true; render();
        const res = await apiFetch('/api/v1/ap/monitoring', { method });
        busy = false;
        if (res && res.ok) {
            const j = await res.json();
            state = { enabled: j.enabled };
            token = j.token || '';
        }
        render();
    }

    function wire() {
        if (wired) return;
        wired = true;
        $('ap-monitor').addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            if (btn.id === 'apm-make') { confirmRevoke = false; act('POST'); }
            else if (btn.id === 'apm-revoke') {
                if (!confirmRevoke) { confirmRevoke = true; render(); setTimeout(() => { confirmRevoke = false; render(); }, 4000); return; }
                confirmRevoke = false; token = ''; act('DELETE');
            }
        });
    }

    function init() {
        wire();
        state = null; token = ''; confirmRevoke = false; busy = false;
        render();
        load();
        return () => { token = ''; };
    }

    return { init };
})();
