import http.cookiejar
import json
import os
import urllib.error
import urllib.request

BASE = os.environ.get("TEST_BASE_URL", "http://127.0.0.1:8080")
ADMIN = os.environ["TEST_ADMIN_NAME"]
PASSWORD = os.environ["TEST_ADMIN_PASSWORD"]


def client():
    return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def call(opener, path, method="GET", body=None, headers=None, expected=200):
    data = None if body is None else json.dumps(body).encode()
    request_headers = {"Origin": BASE, **(headers or {})}
    if body is not None:
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(BASE + path, data=data, method=method, headers=request_headers)
    try:
        response = opener.open(request, timeout=30)
    except urllib.error.HTTPError as error:
        response = error
    payload = response.read()
    assert response.status == expected, (method, path, response.status, payload[:500])
    content_type = response.headers.get("Content-Type", "")
    return (json.loads(payload) if payload and "application/json" in content_type else payload), response.headers


admin = client()
assert call(admin, "/api/session")[0] is None
call(admin, "/healthz")
call(admin, "/readyz")
session, _ = call(admin, "/api/login", "POST", {"username": ADMIN, "password": PASSWORD})
assert session["role"] == "admin"
call(admin, "/api/admin/users/" + session["id"], "PATCH", {"name": "Blocked"},
     headers={"Origin": "https://cross-origin.invalid"}, expected=403)
call(admin, "/api/admin/users/" + session["id"], "PATCH", {"role": "user"}, expected=409)
call(admin, "/api/admin/users/" + session["id"], "DELETE", expected=409)
library, _ = call(admin, "/api/library")
assert library["items"], "Scanner has not indexed test media yet"
feed, _ = call(admin, "/api/library?scope=all")
assert {item["id"] for item in feed["items"]} == {item["id"] for item in library["items"]}
root, _ = call(admin, "/api/library?scope=folder")
assert all(item["folderId"] == "" for item in root["items"])
assert {item["id"] for item in root["items"]} == {item["id"] for item in feed["items"] if item["folderId"] == ""}
for folder in root["folders"]:
    children, _ = call(admin, "/api/library?scope=folder&folder=" + folder["id"])
    assert all(item["folderId"] == folder["id"] for item in children["items"])
    assert all(child["parentId"] == folder["id"] for child in children["folders"])
call(admin, "/api/library?scope=invalid", expected=400)
video = next(item for item in library["items"] if item["kind"] == "video")
assert video["duration"] > 0, video
assert video["resolution"], video
content, headers = call(admin, video["src"], headers={"Range": "bytes=0-99"}, expected=206)
assert len(content) == 100
assert headers["Content-Range"].startswith("bytes 0-99/")
empty, headers = call(admin, video["src"], "HEAD")
assert empty == b"" and int(headers["Content-Length"]) > 100
call(admin, video["src"], headers={"Range": "bytes=999999999999-"}, expected=416)
thumbnail, headers = call(admin, video["image"])
assert headers["Content-Type"].startswith("image/jpeg"), headers
assert len(thumbnail) > 100
call(admin, "/api/items/" + video["id"] + "/progress", "PUT", {"progress": 12})
assert call(admin, "/api/items/" + video["id"])[0]["progress"] == 12
call(admin, "/api/items/" + video["id"] + "/progress", "PUT", {"progress": 5})
assert call(admin, "/api/items/" + video["id"])[0]["progress"] == 5

username = "smoke-" + os.urandom(5).hex()
user_password = "smoke-password-" + os.urandom(8).hex()
created, _ = call(admin, "/api/admin/users", "POST", {
    "username": username, "name": "Smoke User", "password": user_password, "role": "user", "active": True,
}, expected=201)
user_id = created["id"]
user = client()
call(user, "/api/login", "POST", {"username": username, "password": user_password})
assert call(user, "/api/items/" + video["id"])[0]["progress"] == 0
call(user, "/api/admin/users", expected=403)
call(admin, "/api/admin/users/" + session["id"], "PATCH", {"active": False}, expected=409)
call(admin, "/api/admin/users/" + user_id, "PATCH", {"active": False})
call(user, video["src"], expected=401)
call(user, video["image"], expected=401)
call(user, "/api/login", "POST", {"username": username, "password": user_password}, expected=401)
call(admin, "/api/admin/users/" + user_id, "PATCH", {"active": True})
call(user, "/api/login", "POST", {"username": username, "password": user_password})
new_password = user_password + "-reset"
call(admin, "/api/admin/users/" + user_id, "PATCH", {"password": new_password})
call(user, "/api/library", expected=401)
call(user, "/api/login", "POST", {"username": username, "password": user_password}, expected=401)
call(user, "/api/login", "POST", {"username": username, "password": new_password})
call(admin, "/api/admin/users/" + user_id, "DELETE")
call(user, "/api/library", expected=401)
call(admin, "/api/logout", "POST")
call(admin, video["src"], expected=401)
print("Backend HTTP smoke passed: metadata, JPEG thumbnail, range/HEAD/416, progress isolation/rewind, admin CRUD, revocation and last-admin protection.")
