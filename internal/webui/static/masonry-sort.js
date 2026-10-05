(function () {
  "use strict";

  window.PortalMasonrySort = {
    init: function (config) {
      var list = config.list;
      var cards = config.cards.slice();
      var scope = list.getAttribute("data-masonry-key");
      var csrf = list.getAttribute("data-masonry-csrf");
      var handles = cards.map(function (card) { return card.querySelector("[data-masonry-handle]"); });
      var status = list.querySelector("[data-masonry-status]") || list.parentElement && list.parentElement.querySelector("[data-masonry-status]");
      var drag = null;
      var pendingSave = null, saving = false;
      if (!window.PointerEvent || cards.length < 2 || handles.some(function (handle) { return !handle; })) return { cancel: function () {} };
      var labels = handles.map(function (handle) { return handle.getAttribute("aria-label"); });

      function cardKey(card) { return card.getAttribute("data-masonry-card"); }
      function message(text, failed) {
        if (!status) return;
        status.textContent = text;
        if (status.classList) status.classList.toggle("masonry-layout-error", !!failed);
      }
      function saveOrder(order) {
        pendingSave = order.map(cardKey);
        message("正在保存共享排列…", false);
        pumpSave();
      }
      function pumpSave() {
        if (saving || !pendingSave) return;
        var order = pendingSave;
        pendingSave = null;
        if (!window.fetch || !csrf) {
          message("共享排列未保存，请刷新后重试。", true);
          return;
        }
        saving = true;
        var body = new URLSearchParams();
        body.set("csrf_token", csrf);
        body.set("scope", scope);
        body.set("order", JSON.stringify(order));
        Promise.resolve().then(function () {
          return window.fetch("/admin/upstreams/layout", {
            method: "POST", credentials: "same-origin", redirect: "error", keepalive: true,
            headers: { "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8", "Accept": "application/json" },
            body: body.toString()
          });
        }).then(function (response) {
          if (!response.ok) throw new Error("save failed");
          return response.json();
        }).then(function (result) {
          if (!result || result.ok !== true) throw new Error("save failed");
          if (!pendingSave) message("共享排列已保存", false);
        }).catch(function () {
          if (!pendingSave) message("共享排列保存失败，当前调整尚未同步；请重试或刷新。", true);
        }).then(function () {
          saving = false;
          pumpSave();
        });
      }
      function apply(order, save, focusCard) {
        cards = order.slice();
        // Do not move DOM nodes: alias_N fields are bound to the original model order.
        config.applyOrder(cards);
        cards.forEach(function (card, index) {
          var handle = card.querySelector("[data-masonry-handle]");
          handle.setAttribute("aria-label", labels[config.cards.indexOf(card)] + "，第 " + (index + 1) + " 张，共 " + cards.length + " 张");
        });
        if (save) {
          saveOrder(cards);
        }
        if (focusCard) focusCard.querySelector("[data-masonry-handle]").focus({ preventScroll: true });
      }
      try {
        var saved = JSON.parse(list.getAttribute("data-masonry-order"));
        if (Array.isArray(saved)) {
          var restored = [];
          saved.forEach(function (id) {
            var card = cards.find(function (item) { return cardKey(item) === id; });
            if (card && restored.indexOf(card) < 0) restored.push(card);
          });
          cards.forEach(function (card) { if (restored.indexOf(card) < 0) restored.push(card); });
          apply(restored, false);
        }
      } catch (_) {}
      handles.forEach(function (handle) { handle.hidden = false; });

      function clearTarget() {
        if (drag && drag.target) drag.target.classList.remove("is-masonry-drop-target");
      }
      function cancel() { finish(false); }
      function cancelPointer(event) {
        if (drag && event.pointerId === drag.pointerId) cancel();
      }
      function finish(commit) {
        if (!drag) return;
        var current = drag;
        clearTarget();
        drag = null;
        current.card.style.transform = "";
        current.card.classList.remove("is-masonry-dragging");
        document.body.classList.remove("is-masonry-sorting");
        try { current.handle.releasePointerCapture(current.pointerId); } catch (_) {}
        if (commit && current.started && current.target) {
          var from = cards.indexOf(current.card);
          var to = cards.indexOf(current.target);
          if (from !== to) {
            var order = cards.slice();
            order.splice(from, 1);
            order.splice(to, 0, current.card);
            apply(order, true, current.card);
          }
        }
      }
      function move(event) {
        if (!drag || event.pointerId !== drag.pointerId) return;
        var dx = event.clientX - drag.startX, dy = event.clientY - drag.startY;
        if (!drag.started && Math.hypot(dx, dy) < 5) return;
        drag.started = true;
        event.preventDefault();
        drag.card.classList.add("is-masonry-dragging");
        drag.card.style.transform = "translate(" + dx + "px," + dy + "px)";
        clearTarget();
        drag.target = null;
        // Use a card's real bounds, not a nearest-card guess outside the list.
        cards.some(function (card) {
          if (card === drag.card) return false;
          var rect = card.getBoundingClientRect();
          if (event.clientX >= rect.left && event.clientX <= rect.right && event.clientY >= rect.top && event.clientY <= rect.bottom) {
            drag.target = card;
            card.classList.add("is-masonry-drop-target");
            return true;
          }
          return false;
        });
      }
      handles.forEach(function (handle, index) {
        var card = config.cards[index];
        handle.addEventListener("pointerdown", function (event) {
          if (event.isPrimary === false || event.button !== 0 || drag || !list.classList.contains("is-masonry")) return;
          event.preventDefault();
          drag = { card: card, handle: handle, pointerId: event.pointerId, startX: event.clientX, startY: event.clientY, started: false, target: null };
          document.body.classList.add("is-masonry-sorting");
          try { handle.setPointerCapture(event.pointerId); } catch (_) { cancel(); }
        });
        handle.addEventListener("pointermove", move);
        handle.addEventListener("pointerup", function (event) {
          if (!drag || event.pointerId !== drag.pointerId) return;
          move(event);
          finish(true);
        });
        handle.addEventListener("pointercancel", cancelPointer);
        handle.addEventListener("lostpointercapture", cancelPointer);
        handle.addEventListener("click", function (event) { event.preventDefault(); });
        handle.addEventListener("keydown", function (event) {
          var from = cards.indexOf(card), to = from;
          if (event.key === "ArrowLeft" || event.key === "ArrowUp") to--;
          else if (event.key === "ArrowRight" || event.key === "ArrowDown") to++;
          else if (event.key === "Home") to = 0;
          else if (event.key === "End") to = cards.length - 1;
          else return;
          event.preventDefault();
          if (to < 0 || to >= cards.length || to === from || drag) return;
          var order = cards.slice();
          order.splice(from, 1);
          order.splice(to, 0, card);
          apply(order, true, card);
        });
      });
      document.addEventListener("keydown", function (event) {
        if (drag && event.key === "Escape") { event.preventDefault(); cancel(); }
      });
      window.addEventListener("blur", cancel);
      window.addEventListener("resize", cancel);
      window.addEventListener("scroll", cancel, true);
      return { cancel: cancel };
    }
  };
})();
