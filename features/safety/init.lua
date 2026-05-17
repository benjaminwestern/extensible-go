-- Safety feature pack: wrap a core policy seam.
-- This is the small proof that Lua can take over a core decision point.

registry:wrap("core.policy", function(existing)
    return {
        Check = function(self, action)
            if action == "dangerous" or action == "delete-all" then
                return { allow = false, reason = "blocked by Lua safety policy" }
            end
            return existing:Check(action)
        end
    }
end)

app:on("input", function(event, ctx)
    local decision = ctx.check(event.text)
    ctx.print("event input:", event.text, "allow=", decision.allow, "reason=", decision.reason)
end)
