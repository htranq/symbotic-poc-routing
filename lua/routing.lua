function envoy_on_request(handle)
  local path = handle:headers():get(":path") or ""
  handle:logInfo("Lua: /join path " .. path)

  local client_id = nil
  local qpos = string.find(path, "?", 1, true)
  if qpos then
    local qs = string.sub(path, qpos + 1)
    for key, val in string.gmatch(qs, "([^&=?]+)=([^&=?]+)") do
      if key == "client_id" then
        client_id = val
        break
      end
    end
  end

  if client_id == nil then
    handle:respond({[":status"] = "400", ["content-type"] = "text/plain"}, "missing client_id")
    return
  end

  handle:logInfo("Lua: resolving client_id=" .. client_id)
  local req_headers = {
    [":method"] = "GET",
    [":path"] = "/where?client_id=" .. client_id,
    [":authority"] = "resolver",
  }

  local ok, resp_headers, resp_body = pcall(handle.httpCall, handle, "resolver", req_headers, "", 1000)
  if not ok or resp_headers == nil then
    handle:respond({[":status"] = "502", ["content-type"] = "text/plain"}, "resolver httpCall failed")
    return
  end

  local status = nil
  if resp_headers ~= nil then
    if resp_headers.get then
      status = resp_headers:get(":status")
    else
      status = resp_headers[":status"]
    end
  end
  handle:logInfo("Lua: resolver status(raw)=" .. tostring(status))
  if status ~= "200" then
    handle:respond({[":status"] = "502", ["content-type"] = "text/plain"}, "resolver returned " .. tostring(status))
    return
  end

  local hostport = resp_body and string.match(resp_body, '"hostport"%s*:%s*"([^"]+)"') or nil
  handle:logInfo("Lua: parsed hostport=" .. tostring(hostport))
  if not hostport then
    handle:respond({[":status"] = "502", ["content-type"] = "text/plain"}, "no hostport in resolver body")
    return
  end

  local hdr = handle:headers()
  hdr:replace(":authority", hostport)
end
