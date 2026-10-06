// web/static/scripts/first_run.js
// Instant feedback only; the server re-validates everything on submit.

document.addEventListener('DOMContentLoaded', () => {
    const password = document.getElementById('password');
    const confirm = document.getElementById('confirm_password');
    if (!password || !confirm) return;

    function checkMatch() {
        confirm.setCustomValidity(
            confirm.value && confirm.value !== password.value ? 'Passwords do not match.' : ''
        );
    }

    password.addEventListener('input', checkMatch);
    confirm.addEventListener('input', checkMatch);
});
