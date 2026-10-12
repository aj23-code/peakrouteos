// web/static/scripts/backup.js
// Backup and restore page: make one encrypted backup file, open one and see what is in it before anything changes, restore the parts
// you choose (the settings become the draft), and the copies the router keeps of itself to undo a restore. docs/backup.md.

window.BackupModule = (function () {
    let info = { copies: [], min_passphrase: 10, history_bytes: 0, editable: true };
    let mk = { pass: '', confirm: '', history: false, busy: false, error: null };      // making a backup
    let rs = { file: null, fileName: '', pass: '', busy: false, error: null, report: null, pick: {}, keepPorts: false, confirm: false, result: null };
    let confirmCopy = null;                                                           // 'restore:<id>' or 'delete:<id>', until clicked again
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const dateOf = (s) => new Date(s * 1000).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    const SECTION = { settings: 'Settings', admins: 'Administrators', certificates: 'Certificates', history: 'History' };
    const sectionsText = (list) => (list || []).map(s => SECTION[s] || s).join(', ');
    const sizeText = (b) => (b >= 1048576 ? (b / 1048576).toFixed(1) + ' MB' : Math.max(1, Math.round(b / 1024)) + ' kB');

    function showMessage(text, kind) {
        const box = $('backup-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    async function errorText(res, fallback) {
        if (!res) return 'The router did not answer. Try again.';
        try { const e = await res.json(); return e.error || fallback; } catch (e) { return fallback; }
    }

    // ---------------------------------------------------------------- making a backup

    function makeHtml() {
        const short = mk.pass.length > 0 && mk.pass.length < info.min_passphrase;
        const differ = mk.confirm.length > 0 && mk.pass !== mk.confirm;
        const ok = mk.pass.length >= info.min_passphrase && mk.pass === mk.confirm;
        return `<section class="card" id="bk-make">
            <div class="net-card-head"><h2>Back up</h2></div>
            <p class="form-hint">One file with the settings (networks, firewall, DNS, VPN with all its keys, passwords for Dynamic DNS and Wi-Fi), the administrators' accounts, and the certificates (the web authority and its key). It is <strong>always encrypted</strong> with a passphrase you choose, because it holds every private key. <strong>Nobody can recover a lost passphrase</strong>, not even the maker of this software: write it down somewhere safe. The settings in the backup are the ones that are running, not changes still in the draft.</p>
            <div class="form-grid">
                <div class="form-field"><label for="bk-pass">Passphrase</label><input type="password" id="bk-pass" value="${escapeHtml(mk.pass)}" autocomplete="new-password" placeholder="at least ${info.min_passphrase} characters"${mk.busy ? ' disabled' : ''}>
                    ${short ? `<p class="field-error">At least ${info.min_passphrase} characters.</p>` : ''}</div>
                <div class="form-field"><label for="bk-confirm">The same again</label><input type="password" id="bk-confirm" value="${escapeHtml(mk.confirm)}" autocomplete="new-password"${mk.busy ? ' disabled' : ''}>
                    ${differ ? '<p class="field-error">The two are not the same.</p>' : ''}</div>
            </div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" id="bk-history"${mk.history ? ' checked' : ''}${mk.busy ? ' disabled' : ''}><span><strong>Include the history</strong><span class="form-hint">Traffic, internet connection quality and the firewall log (now about ${sizeText(info.history_bytes)}). Not needed to set a router up again, and it makes the file bigger.</span></span></label></div>
            ${mk.error ? `<div class="form-message" role="alert">${escapeHtml(mk.error)}</div>` : ''}
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="bk-download"${ok && !mk.busy ? '' : ' disabled'}>${mk.busy ? 'Making the backup…' : 'Download the backup'}</button></div>
        </section>`;
    }

    async function download() {
        mk.busy = true; mk.error = null; render();
        const res = await apiFetch('/api/v1/backup/create', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ passphrase: mk.pass, confirm: mk.confirm, history: mk.history }) });
        mk.busy = false;
        if (!res || !res.ok) { mk.error = await errorText(res, 'The backup could not be made.'); render(); return; }
        const m = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') || '');
        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url; a.download = m ? m[1] : 'peakroute.prbackup';
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
        mk = { pass: '', confirm: '', history: mk.history, busy: false, error: null };
        render();
        showMessage(`The backup is saved as ${a.download}. Keep it and the passphrase apart, somewhere safe: this file opens everything.`, 'ok');
    }

    // ---------------------------------------------------------------- restoring

    function reviewHtml(st) {
        const rows = (st.changes || []).slice(0, 40).map(c => `<tr><th>${escapeHtml(c.label)}</th><td><span class="cfg-old">${escapeHtml(c.old)}</span> &rarr; <span class="cfg-new">${escapeHtml(c.new)}</span></td></tr>`).join('');
        const more = (st.changes || []).length > 40 ? `<p class="form-hint">… and ${(st.changes || []).length - 40} more.</p>` : '';
        return rows ? `<table class="config-review-table"><tbody>${rows}</tbody></table>${more}` : '<p class="form-hint">The settings in the backup are the same as the ones running now.</p>';
    }

    function reportHtml() {
        const r = rs.report;
        const pick = rs.pick;
        const parts = [];
        if (r.settings) {
            const st = r.settings;
            const portsNote = (st.missing_ports || []).length
                ? `<p class="form-hint net-warn">This router does not have the ports ${st.missing_ports.map(p => `<code>${escapeHtml(p)}</code>`).join(', ')} that the backup uses (it comes from a router with other ports).</p>` : '';
            parts.push(`<div class="bk-part"><label class="toggle toggle--block"><input type="checkbox" data-pick="settings"${pick.settings ? ' checked' : ''}${st.valid ? '' : ' disabled'}><span><strong>Settings</strong><span class="form-hint">Networks, firewall, DNS, VPN and the rest. They become the <strong>draft</strong>: you review and commit them from the bar at the top, with the usual confirmation, so a mistake can still be undone.${st.draft_waits ? ' <span class="net-warn">The changes now waiting in the draft are replaced.</span>' : ''}</span></span></label>
                ${portsNote}
                <label class="toggle toggle--block bk-sub"><input type="checkbox" id="bk-keep-ports"${rs.keepPorts ? ' checked' : ''}><span><strong>Keep this router's own ports</strong><span class="form-hint">Which port is the internet connection, which are LAN ports and what each carries, and the Wi-Fi adapters stay as they are here; everything else comes from the backup. ${(st.missing_ports || []).length ? 'Recommended: the ports differ.' : 'Use it when restoring onto other hardware.'}</span></span></label>
                ${st.valid ? '' : `<div class="form-message" role="alert">These settings cannot be restored${rs.keepPorts ? '' : ' as they are (try keeping this router\'s own ports)'}:<ul>${(st.problems || []).map(p => `<li>${escapeHtml(p)}</li>`).join('')}</ul></div>`}
                <details class="vpn-details"${(st.changes || []).length && (st.changes || []).length <= 6 ? ' open' : ''}><summary>What would change (${(st.changes || []).length})</summary>${reviewHtml(st)}</details></div>`);
        }
        if (r.admins) {
            const a = r.admins;
            parts.push(`<div class="bk-part"><label class="toggle toggle--block"><input type="checkbox" data-pick="admins"${pick.admins ? ' checked' : ''}${a.valid ? '' : ' disabled'}><span><strong>Administrators</strong><span class="form-hint">The ${a.accounts.length} account${a.accounts.length === 1 ? '' : 's'} in the backup (${a.accounts.map(x => escapeHtml(x.name) + (x.role === 'admin' ? '' : ', read-only')).join('; ')}) <strong>replace the ${a.current} here, passwords included. Everyone is signed out, you too.</strong> A backup that is old holds the passwords of that day.</span></span></label>
                ${a.valid ? '' : `<div class="form-message" role="alert"><ul>${a.problems.map(p => `<li>${escapeHtml(p)}</li>`).join('')}</ul></div>`}</div>`);
        }
        if (r.certificates) {
            const c = r.certificates;
            parts.push(`<div class="bk-part"><label class="toggle toggle--block"><input type="checkbox" data-pick="certificates"${pick.certificates ? ' checked' : ''}${c.valid ? '' : ' disabled'}><span><strong>Certificates</strong><span class="form-hint">${c.authority ? `The web authority <code class="vpn-key">${escapeHtml(c.authority.fingerprint.slice(0, 23))}…</code> with its key, so that devices that trusted it still do` : 'The certificate'}${c.custom ? ', and the custom certificate you uploaded' : ''}. They apply at once.</span></span></label>
                ${c.valid ? '' : `<div class="form-message" role="alert"><ul>${c.problems.map(p => `<li>${escapeHtml(p)}</li>`).join('')}</ul></div>`}</div>`);
        }
        if ((r.history || []).length) {
            parts.push(`<div class="bk-part"><label class="toggle toggle--block"><input type="checkbox" data-pick="history"${pick.history ? ' checked' : ''}><span><strong>History</strong><span class="form-hint">${r.history.map(h => escapeHtml(h.name) + ' (' + sizeText(h.size) + ')').join(', ')}. It replaces the history here and comes back <strong>when the router next starts</strong>.</span></span></label></div>`);
        }
        const any = Object.values(pick).some(Boolean);
        return `<div class="bk-open"><p class="form-hint">A backup of <strong>${escapeHtml(r.hostname || 'a router')}</strong> made ${escapeHtml(dateOf(r.created))}${r.software ? ' by ' + escapeHtml(r.software) : ''}. Choose what to restore. Before anything is changed the router keeps a copy of what it has now (below), so a restore can be undone.</p>
            ${parts.join('')}
            ${rs.error ? `<div class="form-message" role="alert">${escapeHtml(rs.error)}</div>` : ''}
            ${!info.editable ? '<p class="form-hint net-warn">A change is being applied or waits for confirmation: confirm or revert it before restoring.</p>' : ''}
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--ghost" id="bk-close"${rs.busy ? ' disabled' : ''}>Close this backup</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="bk-restore"${any && !rs.busy && info.editable ? '' : ' disabled'}>${rs.busy ? 'Restoring…' : (rs.confirm ? 'Click again to restore' : 'Restore the chosen')}</button></div></div>`;
    }

    function resultHtml() {
        const r = rs.result;
        const what = r.done.map(d => SECTION[d] || d).join(', ');
        const lines = [];
        if (r.done.includes('settings')) lines.push(`The settings are in the draft${r.draft_replaced ? ' (they replaced the changes that were waiting)' : ''}. <strong>Review and commit them from the bar at the top.</strong>`);
        if (r.done.includes('certificates')) lines.push('The certificates are in use. A browser may warn once if the router\'s certificate changed.');
        if (r.signed_out) lines.push('The administrators were replaced and everyone is signed out. <a href="/login">Sign in again</a> with an account from the backup.');
        if (r.needs_restart) lines.push('The history comes back the next time the router starts (System, Settings, Power: restart).');
        return `<section class="card"><div class="net-card-head"><h2>Restored</h2><span class="tag tag-ok">${escapeHtml(what)}</span></div>
            <ul class="bk-list">${lines.map(l => `<li>${l}</li>`).join('')}</ul>
            <p class="form-hint">A copy of what this router had before is kept under <em>Copies on this router</em>, below.</p>
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--ghost" id="bk-result-close">OK</button></div></section>`;
    }

    function restoreHtml() {
        if (rs.result) return resultHtml();
        return `<section class="card" id="bk-restore-card">
            <div class="net-card-head"><h2>Restore from a backup</h2></div>
            <p class="form-hint">Choose a backup file and its passphrase. The router opens it and shows what is in it and what would change; <strong>nothing is changed until you press Restore</strong>. Use it to set up a replacement router, or to go back.</p>
            ${rs.report ? reportHtml() : `<div class="form-grid">
                <div class="form-field"><label for="bk-file">Backup file (.prbackup)</label><input type="file" id="bk-file" accept=".prbackup,application/octet-stream"${rs.busy ? ' disabled' : ''}></div>
                <div class="form-field"><label for="bk-rpass">Passphrase</label><input type="password" id="bk-rpass" value="${escapeHtml(rs.pass)}" autocomplete="off"${rs.busy ? ' disabled' : ''}></div>
            </div>
            ${rs.error ? `<div class="form-message" role="alert">${escapeHtml(rs.error)}</div>` : ''}
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="bk-open"${rs.file && rs.pass && !rs.busy ? '' : ' disabled'}>${rs.busy ? 'Opening…' : 'Open the backup'}</button></div>`}
        </section>`;
    }

    async function openBackup() {
        rs.busy = true; rs.error = null; render();
        const form = new FormData();
        form.append('file', rs.file);
        form.append('passphrase', rs.pass);
        const res = await apiFetch('/api/v1/backup/open', { method: 'POST', body: form });
        rs.busy = false;
        if (!res || !res.ok) { rs.error = await errorText(res, 'The backup could not be opened.'); render(); return; }
        const report = await res.json();
        rs.pass = '';
        rs.report = report;
        rs.keepPorts = !!(report.settings && report.settings.keep_ports);
        rs.pick = { settings: !!(report.settings && report.settings.valid), admins: !!(report.admins && report.admins.valid), certificates: !!(report.certificates && report.certificates.valid), history: false };
        rs.confirm = false;
        render();
    }

    async function preview() {
        const res = await apiFetch('/api/v1/backup/preview', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: rs.report.token, keep_ports: rs.keepPorts }) });
        if (!res || !res.ok) { rs.error = await errorText(res, 'The backup is no longer open: open the file again.'); render(); return; }
        rs.report = await res.json();
        if (rs.report.settings && !rs.report.settings.valid) rs.pick.settings = false;
        else if (rs.report.settings && rs.report.settings.valid && !rs.pick.settings) rs.pick.settings = true;
        render();
    }

    async function restore() {
        if (!rs.confirm) {
            rs.confirm = true; render();
            setTimeout(() => { if (rs.confirm && !rs.busy) { rs.confirm = false; render(); } }, 5000);
            return;
        }
        rs.confirm = false; rs.busy = true; rs.error = null; render();
        const body = Object.assign({ token: rs.report.token, keep_ports: rs.keepPorts }, rs.pick);
        const res = await apiFetch('/api/v1/backup/restore', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        rs.busy = false;
        if (!res || !res.ok) { rs.error = await errorText(res, 'The restore failed.'); render(); return; }
        rs.result = await res.json();
        rs.report = null;
        await load();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        render();
        if (rs.result.signed_out) setTimeout(() => { window.location.href = '/login'; }, 8000);
    }

    // ---------------------------------------------------------------- the copies

    function copiesHtml() {
        const rows = info.copies.map(c => `<div class="fw-row"><div class="fw-main"><div class="fw-name"><strong>${escapeHtml(dateOf(c.created))}</strong> <span class="form-hint">${escapeHtml(c.hostname || '')}</span></div>
            <div class="form-hint fw-text">${escapeHtml(sectionsText(c.sections))} · ${sizeText(c.size)}</div></div>
            <div class="fw-actions"><button type="button" class="cfg-btn cfg-btn--ghost bk-copy-restore" data-id="${escapeHtml(c.id)}"${info.editable ? '' : ' disabled'}>${confirmCopy === 'restore:' + c.id ? 'Click again' : 'Restore this'}</button>
            <button type="button" class="cfg-btn cfg-btn--danger bk-copy-delete" data-id="${escapeHtml(c.id)}">${confirmCopy === 'delete:' + c.id ? 'Click again' : 'Delete'}</button></div></div>`).join('');
        return `<section class="card"><div class="net-card-head"><h2>Copies on this router</h2><span class="tag">${info.copies.length}</span></div>
            <p class="form-hint">Before a restore changes anything, the router keeps a copy of what it had: the settings that were running, the administrators and the certificates (not the history). The last three are kept, here, and cannot be downloaded. <em>Restore this</em> puts one back: the settings become the draft, the rest is at once, and everyone is signed out.</p>
            <div class="fw-list">${rows || '<div class="fw-row fw-empty">No copies yet: one is made the first time you restore.</div>'}</div></section>`;
    }

    async function copyAction(kind, id) {
        const key = kind + ':' + id;
        if (confirmCopy !== key) {
            confirmCopy = key; render();
            setTimeout(() => { if (confirmCopy === key) { confirmCopy = null; render(); } }, 5000);
            return;
        }
        confirmCopy = null;
        const res = kind === 'restore'
            ? await apiFetch(`/api/v1/backup/copies/${encodeURIComponent(id)}/restore`, { method: 'POST' })
            : await apiFetch(`/api/v1/backup/copies/${encodeURIComponent(id)}`, { method: 'DELETE' });
        if (!res || !res.ok) { showMessage(await errorText(res, 'That did not work.'), 'error'); await load(); return; }
        if (kind === 'restore') {
            rs.result = await res.json();
            rs.report = null;
            await load();
            if (window.ConfigBanner) window.ConfigBanner.refresh();
            render();
            if (rs.result.signed_out) setTimeout(() => { window.location.href = '/login'; }, 8000);
        } else { info = await res.json(); render(); }
    }

    // ---------------------------------------------------------------- loading and events

    function render() {
        $('backup-body').innerHTML = makeHtml() + restoreHtml() + copiesHtml();
    }

    async function load() {
        const res = await apiFetch('/api/v1/backup');
        if (res && res.ok) info = await res.json();
        render();
    }

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('backup-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            if (btn.id === 'bk-download') download();
            else if (btn.id === 'bk-open') openBackup();
            else if (btn.id === 'bk-restore') restore();
            else if (btn.id === 'bk-close') { rs.report = null; rs.confirm = false; rs.error = null; render(); }
            else if (btn.id === 'bk-result-close') { rs.result = null; render(); }
            else if (btn.classList.contains('bk-copy-restore')) copyAction('restore', btn.dataset.id);
            else if (btn.classList.contains('bk-copy-delete')) copyAction('delete', btn.dataset.id);
        });
        body.addEventListener('input', (e) => {
            const id = e.target.id;
            if (id === 'bk-pass') { mk.pass = e.target.value; patchMake(); }
            else if (id === 'bk-confirm') { mk.confirm = e.target.value; patchMake(); }
            else if (id === 'bk-rpass') { rs.pass = e.target.value; $('bk-open').disabled = !(rs.file && rs.pass); }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.id === 'bk-history') mk.history = el.checked;
            else if (el.id === 'bk-file') { rs.file = el.files[0] || null; $('bk-open').disabled = !(rs.file && rs.pass); }
            else if (el.id === 'bk-keep-ports') { rs.keepPorts = el.checked; preview(); }
            else if (el.dataset && el.dataset.pick) { rs.pick[el.dataset.pick] = el.checked; rs.confirm = false; render(); }
        });
        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter') return;
            if (e.target.id === 'bk-confirm' && !$('bk-download').disabled) { e.preventDefault(); download(); }
            else if (e.target.id === 'bk-rpass' && !$('bk-open').disabled) { e.preventDefault(); openBackup(); }
        });
    }

    // typing in the passphrase boxes must not redraw them (focus and the cursor would go): only what depends on the text is updated
    function patchMake() {
        const ok = mk.pass.length >= info.min_passphrase && mk.pass === mk.confirm;
        $('bk-download').disabled = !ok;
        const card = $('bk-make');
        card.querySelectorAll('.field-error').forEach(n => n.remove());
        const add = (id, text) => { const f = $(id).closest('.form-field'); const p = document.createElement('p'); p.className = 'field-error'; p.textContent = text; f.appendChild(p); };
        if (mk.pass.length > 0 && mk.pass.length < info.min_passphrase) add('bk-pass', `At least ${info.min_passphrase} characters.`);
        if (mk.confirm.length > 0 && mk.pass !== mk.confirm) add('bk-confirm', 'The two are not the same.');
    }

    function init() {
        wire();
        showMessage('');
        mk = { pass: '', confirm: '', history: false, busy: false, error: null };
        rs = { file: null, fileName: '', pass: '', busy: false, error: null, report: null, pick: {}, keepPorts: false, confirm: false, result: null };
        confirmCopy = null;
        render();
        load();
        return () => { mk.pass = mk.confirm = ''; rs.pass = ''; };
    }

    return { init };
})();
