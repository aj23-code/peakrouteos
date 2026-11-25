// web/static/scripts/filtering.js
// DNS filtering page: which networks have their DNS questions answered by AdGuard Home, whether it is running, and the way into its own
// interface (lists, rules, the query log). Which networks filter is set per network on Local Networks; docs/dns-filtering.md has the model.

window.FilteringModule = (function () {
    let data = null;     // GET /api/v1/filtering
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    function showMessage(text) {
        const box = $('filtering-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
    }

    function stateTag() {
        if (!data.installed) return '<span class="tag tag-warn">Not in this system image</span>';
        if (!data.enabled) return '<span class="tag">Off</span>';
        if (data.running) return '<span class="tag tag-ok">Running</span>';
        return '<span class="tag tag-warn">Not answering</span>';
    }

    function networksHtml() {
        const rows = (data.networks || []).map(n => {
            const tags = [];
            tags.push(n.filter ? '<span class="tag tag-ok">Filtered</span>' : '<span class="tag">Not filtered</span>');
            if (n.filter !== n.running) tags.push('<span class="tag tag-draft">Draft</span>');
            return `<div class="net-row"><strong>${escapeHtml(n.name)}</strong> ${tags.join(' ')}</div>`;
        }).join('');
        return `<h3 class="form-section">Networks</h3>${rows}
            <p class="form-hint">Choose which networks filter with the switch on each network&rsquo;s card on <a href="#" data-goto="networks">Local Networks</a>.</p>`;
    }

    function render() {
        if (!data) { $('filtering-body').innerHTML = '<section class="card"><p class="form-hint">Loading&hellip;</p></section>'; return; }
        const url = `https://${location.hostname}:${data.ui_port || '3000'}/`;
        let note = '';
        if (!data.installed) {
            note = '<p class="form-hint net-warn">This system image does not include AdGuard Home. Update to a version that does (Updates), then turn filtering on for a network.</p>';
        } else if (data.pending) {
            note = '<p class="form-hint">There are changes in the draft. AdGuard Home starts, or stops, when you commit them.</p>';
        } else if (data.enabled && !data.running) {
            note = '<p class="form-hint net-warn">AdGuard Home is not answering. Devices on the filtered networks cannot look up names until it does; it restarts by itself within seconds, and if it keeps failing, look at System logs (peakroute-adguard).</p>';
        } else if (!data.enabled) {
            note = '<p class="form-hint">No network filters yet, so AdGuard Home is not running.</p>';
        }
        const version = data.running && data.version ? `<span class="form-hint">AdGuard Home ${escapeHtml(data.version)}${data.protection_enabled === false ? ' &middot; protection is paused in its interface' : ''}</span>` : '';
        $('filtering-body').innerHTML = `<section class="card" id="filtering-card">
            <div class="net-card-head"><h2>AdGuard Home</h2>${stateTag()}</div>
            ${version}
            ${note}
            <p>${data.running
                ? `<a class="cfg-btn cfg-btn--primary" href="${escapeHtml(url)}" target="_blank" rel="noopener">Open AdGuard Home</a>`
                : '<button type="button" class="cfg-btn cfg-btn--primary" disabled>Open AdGuard Home</button>'}</p>
            <p class="form-hint">Its own interface opens in a new tab on port ${escapeHtml(data.ui_port || '3000')}, signed in with this router&rsquo;s account (it has no accounts of its own), from the default network only. There you choose the block lists, add your own rules and allow-lists, and see which device asked for what. Changes there apply at once and are not part of the draft.</p>
            ${networksHtml()}
            <h3 class="form-section">Good to know</h3>
            <ul class="form-hint">
                <li>It filters by name, so it blocks what the lists name and nothing more. Apps that use their own encrypted DNS (DNS over HTTPS) ask it nothing and are not filtered.</li>
                <li>Questions sent to another DNS server on port 53 from a filtered network are caught and answered by AdGuard Home too.</li>
                <li>Local names, forwarded domains and DNS overrides (the DNS page) still work on filtered networks: what it does not block it asks this router&rsquo;s own DNS.</li>
                <li>It keeps a day of query log and statistics. You can lengthen that in its own settings; it is kept on this router&rsquo;s storage.</li>
            </ul>
        </section>`;
    }

    async function load() {
        const res = await apiFetch('/api/v1/filtering');
        if (!res || !res.ok) { showMessage('The router did not answer. Try again.'); return; }
        data = await res.json();
        showMessage('');
        render();
    }

    function init() {
        showMessage('');
        data = null;
        render();
        load();
        // it starts and stops by itself after a commit
        timer = setInterval(load, 10000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
