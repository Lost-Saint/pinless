document.addEventListener("DOMContentLoaded", () => {
    const images = document.querySelectorAll("[data-image-fallback]");
    const currentOrigin = window.location.origin;

    for (const image of images) {
        image.addEventListener("error", () => {
            const card = image.closest(".pin-card");
            if (card) {
                card.classList.add("is-broken");
            }
        });
    }

    const historyBackLinks = document.querySelectorAll("[data-history-back]");

    for (const link of historyBackLinks) {
        link.addEventListener("click", (event) => {
            if (window.history.length < 2) {
                return;
            }

            if (!document.referrer.startsWith(currentOrigin)) {
                return;
            }

            event.preventDefault();
            window.history.back();
        });
    }
});
