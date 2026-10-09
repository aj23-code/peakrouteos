// web/static/scripts/wireless.js
// Wireless page: the Wi-Fi adapters found and what each can do. With none, the page says so and
// greys out the rest.

window.WirelessModule = (function () {
    let timer = null;
    let report = null;

    const $ = (id) => document.getElementById(id);

    function channelChips(band) {
        const usable = band.channels.filter(c => !c.disabled && !c.no_ir);
        const blocked = band.channels.length - usable.length;
        const chips = usable.map(c =>
            `<span class="chip${c.dfs ? ' chip--tag' : ''}" title="${c.mhz} MHz${c.dfs ? ', shares spectrum with radar: an access point listens before it starts' : ''}">${c.number}${c.dfs ? ' <small>DFS</small>' : ''}</span>`).join('');
        return `<div class="chips">${chips || '<span class="form-hint">No channel is open for an access point right now.</span>'}</div>
            ${blocked ? `<p class="form-hint">${blocked} more ${blocked === 1 ? 'channel is' : 'channels are'} not available here under the current country setting.</p>` : ''}`;
    }

    function bandHtml(band) {
        return `<div class="wifi-band">
            <div class="wifi-band-head"><strong>${escapeHtml(band.name)}</strong>
                <span class="form-hint">widths: ${band.widths.map(w => w + ' MHz').join(', ')}</span></div>
            ${channelChips(band)}
        </div>`;
    }

    function adapterHtml(a) {
        let badge;
        if (!a.capabilities_known) badge = '<span class="tag">Capabilities unknown</span>';
        else if (a.ap_capable) badge = '<span class="tag tag-ok">Can be an access point</span>';
        else badge = '<span class="tag tag-warn">Cannot be an access point</span>';

        return `<section class="card wifi-adapter">
            <div class="net-card-head">
                <h2>${escapeHtml(a.name)}</h2>
                ${badge}
                <span class="tag">${escapeHtml(a.state || 'unknown')}</span>
            </div>
            <dl class="inet-live">
                <div><dt>MAC address</dt><dd><code class="code-mac">${escapeHtml(a.mac || '-')}</code></dd></div>
                <div><dt>Driver</dt><dd>${a.driver ? `<span class="badge-driver">${escapeHtml(a.driver)}</span>` : '-'}</dd></div>
                <div><dt>Radio</dt><dd>${escapeHtml(a.phy || '-')}</dd></div>
                <div><dt>Modes</dt><dd>${a.modes.length ? escapeHtml(a.modes.join(', ')) : '-'}</dd></div>
            </dl>
            ${a.bands.map(bandHtml).join('')}
        </section>`;
    }

    function notes(r) {
        const out = [];
        if (!r.iw_available) {
            out.push('The tool that reads what an adapter can do (<code>iw</code>) is not available on this system, so bands and channels cannot be shown.');
        } else if (r.country_known) {
            const world = r.country === '00';
            out.push(`Regulatory country: <strong>${escapeHtml(r.country)}</strong>${world ? ' (the world setting: the strictest, so most 5 GHz channels are closed. Set a country before starting an access point.)' : ''}`);
        }
        return out.map(n => `<p class="form-hint wifi-note">${n}</p>`).join('');
    }

    function render() {
        if (!report) return;
        const none = report.adapters.length === 0;
        $('wifi-empty').classList.toggle('hidden', !none);
        $('wifi-body').classList.toggle('is-disabled', none);
        $('wifi-body').setAttribute('aria-disabled', String(none));
        $('wifi-notes').innerHTML = none ? '' : notes(report);
        $('wifi-adapters').innerHTML = report.adapters.map(adapterHtml).join('');

        // Wi-Fi networks need an adapter that can be an access point
        const capable = report.adapters.some(a => a.ap_capable);
        $('wifi-ssids').classList.toggle('is-disabled', !capable);
    }

    async function load() {
        const res = await apiFetch('/api/v1/wireless');
        if (!res || !res.ok) return;
        report = await res.json();
        render();
    }

    function init() {
        report = null;
        load();
        timer = setInterval(load, 10000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
