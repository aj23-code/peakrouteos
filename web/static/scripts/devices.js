document.addEventListener('DOMContentLoaded', () => {
    const devicesList = document.getElementById('devices-list');
    const refreshBtn = document.getElementById('refresh-devices-btn');

    async function fetchDevices() {
        try {
            const response = await fetch('/api/v1/system/devices');

            if (response.status === 401) {
                window.location.href = '/login';
                return;
            }

            if (!response.ok) {
                throw new Error(`HTTP error! status: ${response.status}`);
            }

            const data = await response.json();

            // Support direct array or object wrapper { devices: [...] }
            const devices = Array.isArray(data) ? data : (data.devices || []);
            renderDevices(devices);
        } catch (error) {
            console.error('Failed to load physical devices:', error);
            devicesList.innerHTML = `
            <tr>
            <td colspan="7" class="text-center py-4 text-danger">
            <i class="bi bi-exclamation-triangle-fill me-2"></i> Failed to load physical devices. Please try again.
            </td>
            </tr>
            `;
        }
    }

    function renderDevices(devices) {
        if (!devices || devices.length === 0) {
            devicesList.innerHTML = `
            <tr>
            <td colspan="7" class="text-center py-4 text-muted">No physical interfaces detected.</td>
            </tr>
            `;
            return;
        }

        devicesList.innerHTML = devices.map(dev => {
            const isUp = dev.state && dev.state.toLowerCase() === 'up';
            const stateBadge = isUp
            ? '<span class="status-badge badge-up"><i class="bi bi-check-circle-fill me-1"></i>UP</span>'
            : '<span class="status-badge badge-down"><i class="bi bi-dash-circle-fill me-1"></i>DOWN</span>';

            return `
            <tr>
            <td class="ps-4 fw-semibold">
            <i class="bi bi-ethernet me-2 text-primary"></i>${escapeHtml(dev.name)}
            </td>
            <td><code class="code-mac">${escapeHtml(dev.mac || 'N/A')}</code></td>
            <td>${stateBadge}</td>
            <td>${escapeHtml(dev.speed || 'N/A')}</td>
            <td>${dev.mtu || 1500}</td>
            <td><span class="badge-driver">${escapeHtml(dev.driver || 'unknown')}</span></td>
            <td class="pe-4 text-end">
            <button class="btn btn-sm btn-icon" title="Configure Interface Settings" data-configure-device="${escapeHtml(dev.name)}">
            <i class="bi bi-gear-fill"></i>
            </button>
            </td>
            </tr>
            `;
        }).join('');
    }

    function configureDevice(ifaceName) {
        console.log(`Navigating to config for ${ifaceName}`);
    }

    // Delegated handler: the device name travels in a data attribute, never inside inline JS
    devicesList.addEventListener('click', (e) => {
        const btn = e.target.closest('[data-configure-device]');
        if (btn) configureDevice(btn.dataset.configureDevice);
    });

    refreshBtn.addEventListener('click', () => {
        devicesList.innerHTML = `
        <tr>
        <td colspan="7" class="text-center py-4 text-muted">
        <div class="spinner-border spinner-border-sm text-primary me-2" role="status"></div>
        Refreshing hardware status...
        </td>
        </tr>
        `;
        fetchDevices();
    });

    // Initial load
    fetchDevices();
});
