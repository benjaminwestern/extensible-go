-- Hello feature pack: register user-facing commands from Lua.

app:register_command("hello", {
    description = "Say hello from a Lua feature pack",
    handler = function(args, ctx)
        local who = args ~= "" and args or "world"
        ctx.print("hello", who, "from Lua")
    end
})

app:register_command("check", {
    description = "Ask the current core.policy whether an action is allowed",
    handler = function(args, ctx)
        local decision = ctx.check(args)
        ctx.print("allow=", decision.allow, "reason=", decision.reason)
    end
})
