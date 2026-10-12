// web/static/scripts/first_run.js
// The first-run wizard (docs/setup.md): a welcome page with the choice of setting up as a new router or restoring a backup; for a new
// router the region and name, the internet connection, the local network and the administrator, then a review; for a backup the file,
// then what is in it. The server checks every answer again; nothing is saved until the last button.

(function () {
    const $ = (id) => document.getElementById(id);
    const cap = (s) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : '');
    const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

    const NEW_STEPS = [['region', 'Region'], ['internet', 'Internet'], ['lan', 'Local network'], ['admin', 'Administrator'], ['review', 'Finish']];
    const RESTORE_STEPS = [['restore', 'Backup'], ['restore-review', 'Restore']];

    let info = null;
    let flow = null;                 // 'new' or 'restore'
    let step = 'welcome';
    let errs = {};                   // field -> message, for the step on screen
    let general = null;
    let busy = false;
    let touched = { lan: false, dhcp: false };
    const F = {
        hostname: '', country: '', timezone: '',
        wan_port: '', wan_mode: 'dhcp', wan_ip: '', wan_subnet: '255.255.255.0', wan_gateway: '', wan_dns: '', pppoe_username: '', pppoe_password: '',
        lan_ports: [], lan_ip: '', lan_subnet: '255.255.255.0', dhcp: true, dhcp_start: '', dhcp_end: '',
        username: 'admin', password: '', confirm: ''
    };
    const R = { file: null, fileName: '', pass: '', report: null, own: false, wan: '', lan: [], history: false };

    // ---------------------------------------------------------------- talking to the router

    async function post(path, body, form) {
        try {
            const res = await fetch(path, form ? { method: 'POST', body: form } : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
            let data = null;
            try { data = await res.json(); } catch (e) { data = null; }
            return { ok: res.ok, status: res.status, data: data || {} };
        } catch (e) {
            return { ok: false, status: 0, data: { error: 'The router did not answer. Check the connection and try again.' } };
        }
    }

    function payload() {
        const split = (s) => String(s || '').split(/[\s,]+/).filter(Boolean);
        const p = Object.assign({}, F);
        p.wan_dns = F.wan_mode === 'static' ? split(F.wan_dns) : [];
        return p;
    }

    function setErrors(list) {
        errs = {};
        general = null;
        (list || []).forEach((e) => {
            if (!e.field) { general = e.message; } else if (!errs[e.field]) { errs[e.field] = e.message; }
        });
    }

    // ---------------------------------------------------------------- pieces

    function portList() { return (info && info.ports) || []; }
    function herePorts() { return portList().filter((p) => p.here).map((p) => p.name); }

    function field(label, name, control, hint, wide) {
        return `<div class="form-field${wide ? ' form-field--wide' : ''}"><label for="f-${name}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${errs[name] ? `<p class="field-error">${esc(cap(errs[name]))}</p>` : ''}</div>`;
    }
    function text(name, opts) {
        opts = opts || {};
        return `<input type="${opts.type || 'text'}" id="f-${name}" data-f="${name}" value="${esc(F[name])}" autocomplete="${opts.auto || 'off'}" spellcheck="false"${opts.placeholder ? ` placeholder="${esc(opts.placeholder)}"` : ''}${errs[name] ? ' aria-invalid="true"' : ''}>`;
    }
    function banner() { return general ? `<div class="form-message" role="alert">${esc(cap(general))}</div>` : ''; }
    function actions(back, nextLabel, nextId) {
        return `<div class="wiz-actions"><button type="button" class="cfg-btn cfg-btn--ghost" data-go="${back}"${busy ? ' disabled' : ''}>Back</button><button type="submit" class="cfg-btn cfg-btn--primary" id="${nextId || 'wiz-next'}"${busy ? ' disabled' : ''}>${nextLabel || 'Next'}</button></div>`;
    }
    function portTag(p) {
        return `${p.state === 'up' ? '<span class="wiz-tag wiz-tag--up">cable</span>' : ''}${p.here ? '<span class="wiz-tag">you are connected through this</span>' : ''}${p.internet && !p.here ? '<span class="wiz-tag">internet now</span>' : ''}`;
    }

    // ---------------------------------------------------------------- the pages

    function welcome() {
        return `<div class="wiz-body"><h2>Welcome</h2>
            <p class="form-hint">This router has not been set up yet. It takes a few minutes: where it is, how it reaches the internet, the network it makes at home, and who may run it. Nothing changes until the last page.</p>
            <div class="wiz-choices">
                <button type="button" class="wiz-choice" data-flow="new"><strong>Set up as a new router</strong><span>Answer a few questions.</span></button>
                <button type="button" class="wiz-choice" data-flow="restore"><strong>Restore from a backup</strong><span>Use a backup file from another PeakRoute router (or this one before it was reinstalled).</span></button>
            </div></div>`;
    }

    function region() {
        const countries = info.countries.map((c) => `<option value="${esc(c.code)}"${F.country === c.code ? ' selected' : ''}>${esc(c.name)}</option>`).join('');
        const mine = F.country ? info.timezones.filter((z) => z.country === F.country) : [];
        const opt = (z) => `<option value="${esc(z.name)}"${F.timezone === z.name ? ' selected' : ''}>${esc(z.name)}</option>`;
        const zones = (mine.length ? `<optgroup label="In this country">${mine.map(opt).join('')}</optgroup><optgroup label="All time zones">` : '<optgroup label="All time zones">') + info.timezones.map(opt).join('') + '</optgroup>';
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>Where is this router?</h2>
            <p class="form-hint">The country sets what the Wi-Fi radios are allowed to do (channels and power are regulated), the time zone sets the clock for the logs and schedules.</p>
            ${banner()}<div class="form-grid">
            ${field('Country', 'country', `<select id="f-country" data-f="country" data-rerender="1"><option value="">Choose…</option>${countries}</select>`)}
            ${field('Time zone', 'timezone', `<select id="f-timezone" data-f="timezone"><option value="">Choose…</option>${zones}</select>`)}
            ${field('Name of this router', 'hostname', text('hostname', { placeholder: 'peakroute' }), 'Letters, numbers and hyphens. It is how the router is named on the network.', true)}
            </div>${actions('welcome')}</form>`;
    }

    function internet() {
        const ports = portList();
        const here = herePorts();
        const wanOpts = ports.map((p) => `<option value="${esc(p.name)}"${F.wan_port === p.name ? ' selected' : ''}${here.indexOf(p.name) >= 0 ? ' disabled' : ''}>${esc(p.name)}${p.state === 'up' ? ' (cable)' : ''}${here.indexOf(p.name) >= 0 ? ' (you are connected through this)' : ''}</option>`).join('');
        const table = ports.length ? `<table class="wiz-ports"><thead><tr><th>Port</th><th>Address</th><th>Speed</th></tr></thead><tbody>${ports.map((p) => `<tr><td>${esc(p.name)} ${portTag(p)}</td><td>${esc(p.mac)}</td><td>${esc(p.speed || '')}</td></tr>`).join('')}</tbody></table>` : '<div class="wiz-note">No wired network port was found on this machine. A router needs at least two.</div>';
        const mode = F.wan_mode;
        const radio = (v, t, d) => `<label class="toggle toggle--block"><input type="radio" name="wan_mode" data-f="wan_mode" data-rerender="1" value="${v}"${mode === v ? ' checked' : ''}><span><strong>${t}</strong><span class="form-hint">${d}</span></span></label>`;
        let extra = '';
        if (mode === 'static') {
            extra = `<div class="form-grid">${field('IP address', 'wan_ip', text('wan_ip', { placeholder: '203.0.113.10' }))}
                ${field('Netmask', 'wan_subnet', text('wan_subnet', { placeholder: '255.255.255.0 or /24' }))}
                ${field('Gateway', 'wan_gateway', text('wan_gateway', { placeholder: '203.0.113.1' }))}
                ${field('DNS servers', 'wan_dns', text('wan_dns', { placeholder: '1.1.1.1, 9.9.9.9' }), 'Separate with commas.')}</div>`;
        } else if (mode === 'pppoe') {
            extra = `<div class="form-grid">${field('Username', 'pppoe_username', text('pppoe_username'))}
                ${field('Password', 'pppoe_password', text('pppoe_password', { type: 'password', auto: 'new-password' }))}</div>`;
        }
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>How does the router reach the internet?</h2>
            <p class="form-hint">Plug the cable from your modem (or the wall) into the internet port. Ports with a cable are marked.</p>
            ${banner()}${table}
            <div class="form-grid">${field('Internet port', 'wan_port', `<select id="f-wan_port" data-f="wan_port" data-rerender="1"><option value="">Choose…</option>${wanOpts}</select>`, '', true)}</div>
            <div class="net-access">${radio('dhcp', 'Automatic (DHCP)', 'The provider hands out the address. Right for most home and office connections.')}${radio('static', 'A fixed address', 'You were given an address, a gateway and DNS servers.')}${radio('pppoe', 'PPPoE', 'A username and password for the provider (common with DSL and fibre).')}</div>
            ${extra}${actions('region')}</form>`;
    }

    function lan() {
        const ports = portList().filter((p) => p.name !== F.wan_port);
        const here = herePorts();
        const rows = ports.map((p) => {
            const isHere = here.indexOf(p.name) >= 0;
            const on = F.lan_ports.indexOf(p.name) >= 0;
            return `<tr><td><label class="toggle"><input type="checkbox" data-lanport="${esc(p.name)}"${on ? ' checked' : ''}${isHere ? ' disabled' : ''}> ${esc(p.name)}</label> ${portTag(p)}</td><td>${esc(p.mac)}</td></tr>`;
        }).join('');
        const dhcp = F.dhcp ? `<div class="form-grid">${field('First address given out', 'dhcp_start', text('dhcp_start'))}${field('Last address given out', 'dhcp_end', text('dhcp_end'))}</div>` : '';
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>The network at home</h2>
            <p class="form-hint">The ports you choose here are joined into one network, and the router gives it an address. More networks (guests, cameras) can be added later.</p>
            ${banner()}
            <table class="wiz-ports"><thead><tr><th>Local network ports</th><th>Address</th></tr></thead><tbody>${rows || '<tr><td colspan="2">There is no port left for the local network.</td></tr>'}</tbody></table>
            ${errs.lan_ports ? `<p class="field-error">${esc(cap(errs.lan_ports))}</p>` : ''}
            <div class="form-grid">${field('Router address', 'lan_ip', text('lan_ip'))}${field('Netmask', 'lan_subnet', text('lan_subnet'), '255.255.255.0 or /24')}</div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" data-f="dhcp" data-rerender="1"${F.dhcp ? ' checked' : ''}><span><strong>Give addresses to devices automatically (DHCP)</strong><span class="form-hint">Turn off only if another device on this network does it.</span></span></label></div>
            ${dhcp}${actions('internet')}</form>`;
    }

    function admin() {
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>Who runs this router?</h2>
            <p class="form-hint">This account can change everything. More can be added later.</p>
            ${banner()}<div class="form-grid">
            ${field('Username', 'username', text('username', { auto: 'username' }), '3 to 32 letters, numbers, dots, dashes or underscores.', true)}
            ${field('Password', 'password', text('password', { type: 'password', auto: 'new-password' }), `At least ${info.min_password} characters.`)}
            ${field('The same again', 'confirm', text('confirm', { type: 'password', auto: 'new-password' }))}
            </div>${actions('lan')}</form>`;
    }

    function review() {
        const country = (info.countries.find((c) => c.code === F.country) || {}).name || F.country;
        const mode = { dhcp: 'Automatic (DHCP)', static: `Fixed address ${F.wan_ip} / ${F.wan_subnet}, gateway ${F.wan_gateway}`, pppoe: `PPPoE as ${F.pppoe_username}` }[F.wan_mode];
        const row = (k, v) => `<tr><th>${k}</th><td>${esc(v)}</td></tr>`;
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>Ready to set up</h2>
            <p class="form-hint">Check the answers. Pressing the button saves them, creates the account and signs you in.</p>
            ${banner()}
            <table class="config-review-table"><tbody>
            ${row('Name', F.hostname)}${row('Country', country)}${row('Time zone', F.timezone)}
            ${row('Internet port', F.wan_port)}${row('Internet connection', mode)}
            ${row('Local network ports', F.lan_ports.join(', '))}${row('Router address', `${F.lan_ip} / ${F.lan_subnet}`)}
            ${row('Addresses for devices', F.dhcp ? `${F.dhcp_start} to ${F.dhcp_end}` : 'Not given out by this router')}
            ${row('Administrator', F.username)}
            </tbody></table>
            <div class="wiz-note">The network settings are saved as a <strong>draft</strong>, not applied yet. After you are signed in the router shows what will change and applies it when you say so. If the router's address changes, the page follows it, and goes back by itself if you cannot reach it.</div>
            ${actions('admin', busy ? 'Setting up…' : 'Set up this router', 'wiz-finish')}</form>`;
    }

    // ---- restoring

    function restoreFile() {
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>Restore from a backup</h2>
            <p class="form-hint">Choose the backup file and type the passphrase it was made with. It must contain the administrators, or nobody could sign in. Nothing changes until you press Restore on the next page.</p>
            ${banner()}<div class="form-grid">
            <div class="form-field form-field--wide"><label for="r-file">Backup file</label><input type="file" id="r-file" accept=".prbackup,application/octet-stream"></div>
            <div class="form-field form-field--wide"><label for="r-pass">Passphrase</label><input type="password" id="r-pass" value="${esc(R.pass)}" autocomplete="off"></div>
            </div>${actions('welcome', busy ? 'Opening…' : 'Open')}</form>`;
    }

    function restoreReview() {
        const r = R.report;
        const parts = [];
        const sections = (r.sections || []).map((s) => ({ settings: 'settings', admins: 'administrators', certificates: 'certificates', history: 'history' }[s] || s)).join(', ');
        parts.push(`<p class="form-hint">From <strong>${esc(r.hostname || 'a router')}</strong>, made ${esc(new Date(r.created * 1000).toLocaleString())}. It contains: ${esc(sections)}.</p>`);
        let ok = true;
        if (!r.admins) {
            ok = false;
            parts.push('<div class="wiz-note">This backup has no administrators, so nobody could sign in to the restored router. Make a new backup with the administrators included, or set up as a new router.</div>');
        } else {
            if (!r.admins.valid) ok = false;
            parts.push(`<h2>Administrators</h2><ul class="wiz-list">${(r.admins.accounts || []).map((a) => `<li>${esc(a.name)} (${esc(a.role)})</li>`).join('')}</ul>${(r.admins.problems || []).map((x) => `<p class="field-error">${esc(x)}</p>`).join('')}`);
        }
        if (r.settings) {
            const st = r.settings;
            if (!st.valid) ok = false;
            parts.push('<h2>Settings</h2>');
            parts.push((st.problems || []).map((x) => `<p class="field-error">${esc(x)}</p>`).join(''));
            const ch = st.changes || [];
            parts.push(ch.length ? `<p class="form-hint">${ch.length} setting${ch.length === 1 ? '' : 's'} will be set. They are saved as a draft: you review and apply them after signing in.</p>
                <details class="wiz-details"><summary>Show the settings</summary><table class="config-review-table"><tbody>${ch.slice(0, 40).map((c) => `<tr><th>${esc(c.label)}</th><td>${esc(c.new)}</td></tr>`).join('')}</tbody></table>${ch.length > 40 ? `<p class="form-hint">… and ${ch.length - 40} more.</p>` : ''}</details>` : '');
            const own = R.own;
            const here = herePorts();
            const rows = portList().map((p) => {
                const isHere = here.indexOf(p.name) >= 0;
                return `<tr><td>${esc(p.name)} ${portTag(p)}</td><td><input type="radio" name="r-wan" data-rwan="${esc(p.name)}"${R.wan === p.name ? ' checked' : ''}${isHere ? ' disabled' : ''}></td><td><input type="checkbox" data-rlan="${esc(p.name)}"${R.lan.indexOf(p.name) >= 0 ? ' checked' : ''}${isHere ? ' disabled' : ''}></td></tr>`;
            }).join('');
            const miss = (st.missing_ports || []).length ? `<div class="wiz-note">The backup uses ports this router does not have (${esc(st.missing_ports.join(', '))}). The ports of this router are listed below: choose the internet port and the local network ports.</div>` : '';
            parts.push(`${miss}<div class="net-access"><label class="toggle toggle--block"><input type="checkbox" id="r-own"${own || (st.missing_ports || []).length ? ' checked' : ''}${(st.missing_ports || []).length ? ' disabled' : ''}><span><strong>Choose this router's ports myself</strong><span class="form-hint">Otherwise the ports of the backup are used as they are.</span></span></label></div>`);
            if (own || (st.missing_ports || []).length) {
                parts.push(`<table class="wiz-ports"><thead><tr><th>Port</th><th>Internet</th><th>Local network</th></tr></thead><tbody>${rows}</tbody></table>`);
            }
        }
        if (r.certificates) {
            if (!r.certificates.valid) ok = false;
            parts.push(`<h2>Certificates</h2><p class="form-hint">The web certificate and its authority${r.certificates.custom ? ' (your own certificate)' : ''} come back as they were, so browsers that trusted the old router trust this one.</p>${(r.certificates.problems || []).map((x) => `<p class="field-error">${esc(x)}</p>`).join('')}`);
        }
        if ((r.history || []).length) {
            parts.push(`<div class="net-access"><label class="toggle toggle--block"><input type="checkbox" id="r-history"${R.history ? ' checked' : ''}><span><strong>Restore the history too</strong><span class="form-hint">Traffic, connection quality and the firewall log.</span></span></label></div>`);
        }
        if (R.own || (r.settings && (r.settings.missing_ports || []).length)) {
            if (!R.wan || !R.lan.length) ok = false;
        }
        return `<form id="wiz-form" class="wiz-body" novalidate><h2>What is in the backup</h2>${banner()}${parts.join('')}
            ${actions('restore', busy ? 'Restoring…' : 'Restore', 'wiz-restore')}</form>`;
    }

    function done() {
        return `<div class="wiz-body"><h2>The backup is restored</h2>
            <p class="form-hint">Sign in with an administrator account from the backup. The settings are waiting as a draft: after signing in, review them and apply.</p>
            <div class="wiz-actions"><span></span><a class="cfg-btn cfg-btn--primary" href="/login">Sign in</a></div></div>`;
    }

    // ---------------------------------------------------------------- showing

    function render() {
        const sub = { welcome: 'Set up this router', done: 'Restored' }[step] || (flow === 'restore' ? 'Restore from a backup' : 'Set up as a new router');
        $('wiz-sub').textContent = sub;
        const steps = flow === 'restore' ? RESTORE_STEPS : flow === 'new' ? NEW_STEPS : [];
        const idx = steps.findIndex((s) => s[0] === step);
        const bar = $('wiz-steps');
        bar.classList.toggle('hidden', idx < 0);
        bar.innerHTML = steps.map((s, i) => `<li class="${i < idx ? 'done' : i === idx ? 'now' : ''}">${s[1]}</li>`).join('');
        const page = { welcome, region, internet, lan, admin, review, restore: restoreFile, 'restore-review': restoreReview, done }[step];
        $('wiz-body').innerHTML = page();
        bind();
        const first = $('wiz-body').querySelector('[aria-invalid="true"]') || $('wiz-body').querySelector('input:not([type=radio]):not([type=checkbox]):not([type=file]), select');
        if (first && step !== 'welcome') first.focus();
    }

    function go(to) {
        step = to;
        errs = {}; general = null;
        render();
    }

    // ---------------------------------------------------------------- behaviour

    function enterStep(name) {
        if (name === 'lan') {
            if (!touched.lan) {
                const own = F.wan_port;
                F.lan_ports = portList().filter((p) => p.name !== own).map((p) => p.name);
            }
            herePorts().forEach((h) => { if (F.lan_ports.indexOf(h) < 0 && h !== F.wan_port) F.lan_ports.push(h); });
            F.lan_ports = F.lan_ports.filter((n) => n !== F.wan_port);
        }
        go(name);
    }

    function bind() {
        const root = $('wiz-body');
        root.querySelectorAll('[data-flow]').forEach((b) => b.addEventListener('click', () => { flow = b.dataset.flow; enterStep(flow === 'new' ? 'region' : 'restore'); }));
        root.querySelectorAll('[data-go]').forEach((b) => b.addEventListener('click', () => { if (b.dataset.go === 'welcome') flow = null; go(b.dataset.go); }));
        root.querySelectorAll('[data-f]').forEach((el) => {
            const name = el.dataset.f;
            const read = () => (el.type === 'checkbox' ? el.checked : el.value);
            const apply = () => {
                if (el.type === 'radio' && !el.checked) return;
                F[name] = read();
                if (name === 'country') suggestZone();
                if (name === 'wan_port') touched.lan = false;
                if (el.dataset.rerender) { errs = {}; general = null; render(); }
            };
            el.addEventListener(el.tagName === 'SELECT' || el.type === 'checkbox' || el.type === 'radio' ? 'change' : 'input', apply);
            if (name === 'dhcp_start' || name === 'dhcp_end') el.addEventListener('input', () => { touched.dhcp = true; });
            if (name === 'lan_ip') el.addEventListener('change', followAddress);
        });
        root.querySelectorAll('[data-lanport]').forEach((el) => el.addEventListener('change', () => {
            touched.lan = true;
            const n = el.dataset.lanport;
            F.lan_ports = F.lan_ports.filter((x) => x !== n);
            if (el.checked) F.lan_ports.push(n);
        }));
        const form = $('wiz-form');
        if (form) form.addEventListener('submit', (e) => { e.preventDefault(); next(); });

        // restoring
        const file = $('r-file');
        if (file) file.addEventListener('change', () => { R.file = file.files[0] || null; R.fileName = R.file ? R.file.name : ''; });
        const pass = $('r-pass');
        if (pass) pass.addEventListener('input', () => { R.pass = pass.value; });
        const own = $('r-own');
        if (own) own.addEventListener('change', () => { R.own = own.checked; if (R.own && !R.wan) suggestRestorePorts(); render(); if (R.own) previewPorts(); else reopenAsIs(); });
        const hist = $('r-history');
        if (hist) hist.addEventListener('change', () => { R.history = hist.checked; });
        root.querySelectorAll('[data-rwan]').forEach((el) => el.addEventListener('change', () => {
            R.wan = el.dataset.rwan;
            R.lan = R.lan.filter((x) => x !== R.wan);
            previewPorts();
        }));
        root.querySelectorAll('[data-rlan]').forEach((el) => el.addEventListener('change', () => {
            const n = el.dataset.rlan;
            R.lan = R.lan.filter((x) => x !== n);
            if (el.checked) { R.lan.push(n); if (R.wan === n) R.wan = ''; }
            previewPorts();
        }));
    }

    function suggestZone() {
        const mine = info.timezones.filter((z) => z.country === F.country);
        if (!F.timezone || !info.timezones.some((z) => z.name === F.timezone && z.country === F.country)) {
            F.timezone = mine.length === 1 ? mine[0].name : (mine.find((z) => z.name === F.timezone) ? F.timezone : '');
        }
        errs = {}; general = null;
        render();
    }

    // when the router address is typed, the range of addresses follows it until the range has been typed by hand
    function followAddress() {
        F.lan_ip = $('f-lan_ip').value.trim();
        const m = /^(\d+\.\d+\.\d+)\.\d+$/.exec(F.lan_ip);
        if (m && !touched.dhcp) {
            F.dhcp_start = m[1] + '.100';
            F.dhcp_end = m[1] + '.200';
            const a = $('f-dhcp_start'), b = $('f-dhcp_end');
            if (a) a.value = F.dhcp_start;
            if (b) b.value = F.dhcp_end;
        }
    }

    async function next() {
        if (busy) return;
        if (step === 'restore') return openBackup();
        if (step === 'restore-review') return restoreNow();
        if (step === 'review') return finish();
        busy = true;
        const body = Object.assign({ through: step }, payload());
        const res = await post('/api/v1/setup/check', body);
        busy = false;
        if (res.status === 409) { location.href = '/login'; return; }
        if (!res.ok) { setErrors(res.data.errors && res.data.errors.length ? res.data.errors : [{ field: '', message: res.data.error || 'Something is not right.' }]); render(); return; }
        const order = NEW_STEPS.map((s) => s[0]);
        enterStep(order[order.indexOf(step) + 1]);
    }

    async function finish() {
        busy = true; general = null; render();
        const res = await post('/api/v1/setup/apply', payload());
        busy = false;
        if (res.status === 409) { location.href = '/login'; return; }
        if (!res.ok) {
            setErrors(res.data.errors && res.data.errors.length ? res.data.errors : [{ field: '', message: res.data.error || 'The router could not be set up.' }]);
            // go back to the first page that has a problem
            const bad = NEW_STEPS.map((s) => s[0]).find((s) => Object.keys(errs).some((f) => fieldStep(f) === s));
            if (bad && bad !== 'review') { step = bad; }
            render();
            return;
        }
        try { sessionStorage.setItem('pr_open_review', '1'); } catch (e) { /* the review is then opened from the banner */ }
        location.href = res.data.redirect || '/dashboard';
    }

    function fieldStep(f) {
        if (/^(hostname|country|timezone)$/.test(f)) return 'region';
        if (/^(wan|pppoe)/.test(f)) return 'internet';
        if (/^(lan|dhcp)/.test(f)) return 'lan';
        if (/^(username|password|confirm)$/.test(f)) return 'admin';
        return 'review';
    }

    // ---- restoring

    function suggestRestorePorts() {
        R.wan = info.suggest_wan || '';
        R.lan = (info.suggest_lan || []).slice();
    }

    async function openBackup() {
        if (!R.file) { general = 'Choose the backup file.'; render(); return; }
        if (!R.pass) { general = 'Type the passphrase of the backup.'; render(); return; }
        busy = true; general = null; render();
        const fd = new FormData();
        fd.append('file', R.file);
        fd.append('passphrase', R.pass);
        const res = await post('/api/v1/setup/restore/open', null, fd);
        busy = false;
        if (res.status === 409 && !res.data.token) { location.href = '/login'; return; }
        if (!res.ok) { general = res.data.error || 'The backup could not be opened.'; render(); return; }
        R.report = res.data;
        R.pass = '';
        R.own = false; R.wan = ''; R.lan = []; R.history = false;
        if (R.report.settings && (R.report.settings.missing_ports || []).length) { R.own = true; suggestRestorePorts(); await previewPorts(true); }
        go('restore-review');
    }

    async function previewPorts(quiet) {
        if (!R.wan || !R.lan.length) { if (!quiet) render(); return; }
        const res = await post('/api/v1/setup/restore/preview', { token: R.report.token, wan: R.wan, lan: R.lan });
        if (res.ok) { R.report = res.data; general = null; } else { general = res.data.error || 'The ports could not be checked.'; }
        if (!quiet) render();
    }

    // back to the backup's own ports
    async function reopenAsIs() {
        R.wan = ''; R.lan = [];
        const res = await post('/api/v1/setup/restore/preview', { token: R.report.token, wan: '', lan: [] });
        if (res.ok) { R.report = res.data; render(); }
    }

    async function restoreNow() {
        const r = R.report;
        if (!r.admins || !r.admins.valid) { general = 'This backup cannot be restored: see the administrators above.'; render(); return; }
        if (r.settings && !r.settings.valid) { general = 'The settings in this backup cannot be restored as they are: see above.'; render(); return; }
        if (r.certificates && !r.certificates.valid) { general = 'The certificates in this backup cannot be restored: see above.'; render(); return; }
        if (R.own && (!R.wan || !R.lan.length)) { general = 'Choose the internet port and at least one local network port.'; render(); return; }
        busy = true; general = null; render();
        const res = await post('/api/v1/setup/restore/apply', { token: r.token, wan: R.own ? R.wan : '', lan: R.own ? R.lan : [], history: R.history });
        busy = false;
        if (res.status === 409 && /already set up/.test(res.data.error || '')) { location.href = '/login'; return; }
        if (!res.ok) { general = res.data.error || 'The backup could not be restored.'; render(); return; }
        flow = 'restore';
        go('done');
    }

    // ---------------------------------------------------------------- start

    function guessRegion() {
        try {
            const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
            const z = info.timezones.find((x) => x.name === tz);
            if (z) {
                if (!F.timezone) F.timezone = z.name;
                if (!F.country && z.country) F.country = z.country;
            }
        } catch (e) { /* the person chooses */ }
    }

    async function start() {
        let res;
        try { res = await fetch('/api/v1/setup/info', { cache: 'no-store' }); } catch (e) { res = null; }
        if (res && res.status === 409) { location.href = '/login'; return; }
        if (!res || !res.ok) { $('wiz-body').innerHTML = '<div class="form-message" role="alert">The router did not answer. Reload this page to try again.</div>'; return; }
        info = await res.json();
        F.hostname = info.hostname || 'peakroute';
        F.country = info.country || '';
        F.timezone = info.timezone || '';
        F.wan_port = info.suggest_wan || '';
        F.lan_ports = (info.suggest_lan || []).slice();
        F.lan_ip = info.defaults.lan_ip;
        F.lan_subnet = info.defaults.lan_subnet;
        F.dhcp_start = info.defaults.dhcp_start;
        F.dhcp_end = info.defaults.dhcp_end;
        guessRegion();
        render();
    }

    document.addEventListener('DOMContentLoaded', start);
})();
