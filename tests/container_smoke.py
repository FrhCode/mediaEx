import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--image", default="mediaex-backend:test")
parser.add_argument("--browser", action="store_true")
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
suffix = secrets.token_hex(5)
container = "mediaex-smoke-" + suffix
volume = container + "-data"
admin_name = "smokeadmin"
admin_password = secrets.token_urlsafe(24)
origin = None


def docker(*arguments, **kwargs):
    return subprocess.run(["docker", *arguments], check=True, text=True, **kwargs)


def request(path, cookie=None, body=None):
    headers = {"Origin": origin}
    if cookie:
        headers["Cookie"] = cookie
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(origin + path, headers=headers, data=None if body is None else json.dumps(body).encode())
    with urllib.request.urlopen(req, timeout=5) as response:
        return json.load(response), response.headers


def resolve_origin():
    # A restarted container receives a new ephemeral host port.
    global origin
    port = docker("inspect", "--format", '{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}', container,
                  capture_output=True).stdout.strip()
    origin = "http://127.0.0.1:" + port


def wait_ready():
    deadline = time.monotonic() + 120
    cookie = None
    while time.monotonic() < deadline:
        try:
            if cookie is None:
                # Sign in once: repeated attempts would trip the login throttle.
                state = docker("inspect", "--format", "{{.State.Running}}", container, capture_output=True).stdout.strip()
                if state != "true":
                    raise RuntimeError("Backend container exited unexpectedly")
                _, headers = request("/api/login", body={"username": admin_name, "password": admin_password})
                cookie = next(value.split(";", 1)[0] for value in headers.get_all("Set-Cookie") if value.startswith("mediaex_session="))
            library, _ = request("/api/library", cookie)
            if any(item["kind"] == "video" for item in library["items"]):
                return cookie, library
        except (OSError, TimeoutError):
            # The published port rejects connections while the server is still starting.
            pass
        time.sleep(1)
    raise TimeoutError("Backend failed to index fixture media within 120 seconds")


with tempfile.TemporaryDirectory(prefix="mediaex-fixtures-") as media:
    fixture = Path(media)
    fixture.chmod(0o755)
    (fixture / "Movies" / "Trips").mkdir(parents=True)
    shutil.copyfile(root / "frontend/public/media/demo.mp4", fixture / "Movies/Trips/holiday.mp4")
    shutil.copyfile(root / "frontend/public/media/island.jpg", fixture / "island.jpg")
    (fixture / ".mediaex-ready").touch()
    for path in fixture.rglob("*"):
        path.chmod(0o755 if path.is_dir() else 0o644)
    try:
        docker("volume", "create", volume, capture_output=True)
        docker("run", "--detach", "--name", container, "--read-only", "--cap-drop", "ALL",
               "--security-opt", "no-new-privileges:true", "--tmpfs", "/tmp:size=128m,mode=1777",
               "--publish", "127.0.0.1::8080", "--mount", "type=bind,source=" + media + ",target=/media,readonly",
               "--mount", "type=volume,source=" + volume + ",target=/data",
               "--env", "ADMIN_NAME=" + admin_name, "--env", "ADMIN_PASSWORD=" + admin_password,
               "--env", "SCAN_INTERVAL=2s", "--env", "SCAN_SETTLE=0s", "--env", "MEDIA_MARKER=.mediaex-ready",
               "--env", "COOKIE_SECURE=false", args.image, capture_output=True)
        resolve_origin()
        cookie, library = wait_ready()
        video = next(item for item in library["items"] if item["kind"] == "video")
        sprite = video["sprite"]
        assert "?v=" in sprite["url"]
        assert 1 <= sprite["count"] <= 300
        sprite_request = urllib.request.Request(origin + sprite["url"], headers={"Cookie": cookie})
        with urllib.request.urlopen(sprite_request, timeout=10) as response:
            sheet = response.read()
            assert sheet[:2] == b"\xff\xd8", "Sprite must be a real JPEG"
            assert response.headers["Cache-Control"] == "private, no-store"
            etag = response.headers["ETag"]
        sprite_request.add_header("If-None-Match", etag)
        try:
            urllib.request.urlopen(sprite_request, timeout=10)
            raise AssertionError("Expected conditional sprite request to return 304")
        except urllib.error.HTTPError as error:
            assert error.code == 304, error
        for path, auth, expected in [(sprite["url"], False, 401), (sprite["url"].split("?")[0] + "?v=stale", True, 404)]:
            try:
                urllib.request.urlopen(urllib.request.Request(origin + path, headers={"Cookie": cookie} if auth else {}), timeout=10)
                raise AssertionError("Unexpectedly served protected/stale sprite")
            except urllib.error.HTTPError as error:
                assert error.code == expected, error
                assert "no-store" in error.headers["Cache-Control"]
        folder_id = video["folderId"]
        folder, _ = request("/api/library?folder=" + folder_id, cookie)
        assert folder["folder"]["name"] == "Trips"
        assert len(folder.get("breadcrumbs", [])) >= 2, folder
        environment = {**os.environ, "TEST_BASE_URL": origin, "TEST_ADMIN_NAME": admin_name, "TEST_ADMIN_PASSWORD": admin_password}
        subprocess.run([sys.executable, "-I", str(root / "tests/backend_smoke.py")], env=environment, check=True)
        if args.browser:
            subprocess.run(["node", str(root / "tests/browser_e2e.mjs")], env=environment, check=True)
        saved, _ = request("/api/items/" + video["id"], cookie)
        docker("restart", container, capture_output=True)
        resolve_origin()
        cookie, library = wait_ready()
        restored, _ = request("/api/items/" + video["id"], cookie)
        assert restored["progress"] == saved["progress"], restored
        (fixture / ".mediaex-ready").unlink()
        shutil.move(fixture / "Movies/Trips/holiday.mp4", fixture / "preserved-copy.tmp")
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            current, _ = request("/api/items/" + video["id"], cookie)
            assert current["id"] == video["id"]
            time.sleep(1)
        (fixture / ".mediaex-ready").touch()
        (fixture / ".mediaex-ready").chmod(0o644)
        deadline = time.monotonic() + 20
        while True:
            try:
                request("/api/items/" + video["id"], cookie)
            except urllib.error.HTTPError as error:
                assert error.code == 404, error
                break
            if time.monotonic() >= deadline:
                raise TimeoutError("Scanner did not remove missing file after marker returned")
            time.sleep(1)
        shutil.copyfile(root / "frontend/public/media/beach.jpg", fixture / "Movies/Trips/new-photo.jpg")
        (fixture / "Movies/Trips/new-photo.jpg").chmod(0o644)
        deadline = time.monotonic() + 20
        while True:
            indexed, _ = request("/api/library?q=new-photo", cookie)
            if len(indexed["items"]) == 1:
                break
            if time.monotonic() >= deadline:
                raise TimeoutError("Scheduled scanner did not discover new NAS file")
            time.sleep(1)
        print("Container smoke passed: real FFmpeg/sprite JPEG, versioned private cache/ETag/304, protected/stale sprite rejection, recursive read-only NAS, restart persistence, scheduled discovery/deletion and marker-loss preservation.")
    except BaseException:
        subprocess.run(["docker", "logs", "--tail", "100", container], check=False)
        raise
    finally:
        subprocess.run(["docker", "rm", "--force", container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
