// web/static/scripts/app.js

/**
 * Universal API Fetch Wrapper
 * Intercepts 401 Unauthorized responses to bounce expired sessions directly to /login.
 */
async function apiFetch(url, options = {}) {
    try {
        const res = await fetch(url, options);

        if (res.status === 401) {
            window.location.href = '/login';
            return null;
        }

        return res;
    } catch (err) {
        console.error(`[API Error] Request failed for ${url}:`, err);
        return null;
    }
}

/**
 * Formatting Utilities
 */
function formatBytes(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

function formatUptime(seconds) {
    if (!seconds) return '0s';
    const days = Math.floor(seconds / (3600 * 24));
    const hrs = Math.floor((seconds % (3600 * 24)) / 3600);
    const mins = Math.floor((seconds % 3600) / 60);
    const secs = seconds % 60;

    let parts = [];
    if (days > 0) parts.push(`${days}d`);
    if (hrs > 0) parts.push(`${hrs}h`);
    if (mins > 0) parts.push(`${mins}m`);
    parts.push(`${secs}s`);
    return parts.join(' ');
}

/**
 * Subpage Router & View Lifecycle Manager
 */
let activeViewCleanup = null;

function loadView(viewName) {
    // Clean up timers/listeners from the previous view
    if (typeof activeViewCleanup === 'function') {
        activeViewCleanup();
        activeViewCleanup = null;
    }

    // Toggle active tab navigation state
    document.querySelectorAll('.nav-item').forEach(item => {
        item.classList.remove('active');
    });
    const activeNav = document.querySelector(`.nav-item[data-view="${viewName}"]`);
    if (activeNav) activeNav.classList.add('active');

    // Toggle view visibility
    document.querySelectorAll('.view-section').forEach(section => {
        section.classList.add('hidden');
    });
    const targetSection = document.getElementById(`view-${viewName}`);
    if (targetSection) {
        targetSection.classList.remove('hidden');
    }

    // Trigger view initialization module
    switch (viewName) {
        case 'overview':
            if (window.TelemetryModule) {
                activeViewCleanup = window.TelemetryModule.init();
            }
            break;
        case 'dhcp':
            if (window.DHCPModule) {
                activeViewCleanup = window.DHCPModule.init();
            }
            break;
        default:
            console.warn(`No initializer found for view: ${viewName}`);
    }
}

// App Shell Initialization
document.addEventListener('DOMContentLoaded', () => {
    // Event listeners for navigation items
    document.querySelectorAll('.nav-item[data-view]').forEach(navItem => {
        navItem.addEventListener('click', (e) => {
            e.preventDefault();
            const view = navItem.getAttribute('data-view');
            loadView(view);
        });
    });

    // Load default view
    loadView('overview');
});
