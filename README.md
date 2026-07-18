# Implementation Plan: Multiplayer Comic Chat Rooms with Chrome Local LLM (window.ai)

We will extend the "Comic Chat" architecture to support multi-user chat rooms. Users can create a chat room, share a unique link, join with a username, and collaborate in real-time. The text understanding (LLM processing) will happen directly in the browser using Chrome's built-in **Prompt API (Gemini Nano)**. The FastAPI server will act as a WebSocket synchronization hub and Pillow rendering host.

---

## 🎯 Goal Description
Exposing the agent to WhatsApp requires third-party servers. To maintain private, upload-free, and cost-free multiplayer interactions:
1. **Multiplayer Rooms**: Establish room-based chat viewports where users can communicate.
2. **WebSocket Synchronization**: Connect all participants via a real-time WebSocket connection to sync room messages and generated images.
3. **Chrome Built-in AI**: Leverage Chrome Canary/Dev's `window.ai` (Gemini Nano) directly in each user's browser, bypassing backend LLM compute entirely.
4. **Server-Side Render sharing**: When any participant sends a message, their Chrome browser processes it via the local LLM, extracts the panel details, and sends them via WebSocket to the FastAPI server. The server renders the high-quality JPEG and broadcasts it back to all participants in the room.

---

## ⚠️ User Review Required
> [!IMPORTANT]
> **Chrome Local LLM Requirements**: To run Chrome's local LLM, users need Chrome Dev/Canary with the following flags enabled:
> - `chrome://flags/#optimization-guide-on-device-model` (set to `Enabled BypassPrefGestureLimit`)
> - `chrome://flags/#prompt-api-for-gemini-nano` (set to `Enabled`)
>
> If a participant's browser does not support `window.ai` or doesn't have it enabled, the web interface will **gracefully fall back** to a client-side rule-based parser. This ensures the app is immediately testable on any browser without flags.

---

## 🛠️ Proposed Changes

We will introduce a WebSocket connection manager, add WebSockets and page routers to `app/main.py`, and implement a multiplayer HTML client.

### 1. Connection Manager (`app/services/room_manager.py`)
We will create a new service to maintain active WebSocket connections grouped by `room_id`.

#### [NEW] `app/services/room_manager.py`
```python
import logging
from typing import Dict, List
from fastapi import WebSocket

logger = logging.getLogger(__name__)

class RoomConnectionManager:
    """
    Manages active room-specific WebSocket connections.
    """
    def __init__(self):
        # Maps room_id -> List of active WebSockets
        self.active_connections: Dict[str, List[WebSocket]] = {}

    async def connect(self, websocket: WebSocket, room_id: str):
        await websocket.accept()
        if room_id not in self.active_connections:
            self.active_connections[room_id] = []
        self.active_connections[room_id].append(websocket)
        logger.info(f"New connection to room {room_id}. Total connections: {len(self.active_connections[room_id])}")

    def disconnect(self, websocket: WebSocket, room_id: str):
        if room_id in self.active_connections:
            self.active_connections[room_id].remove(websocket)
            if not self.active_connections[room_id]:
                del self.active_connections[room_id]
            logger.info(f"Connection left room {room_id}.")

    async def broadcast(self, message: dict, room_id: str):
        """
        Sends JSON message to all connections in a specific room.
        """
        if room_id in self.active_connections:
            for connection in self.active_connections[room_id]:
                try:
                    await connection.send_json(message)
                except Exception as e:
                    logger.warning(f"Error sending broadcast to connection in room {room_id}: {e}")
```

---

### 2. Main Server Routing (`app/main.py`)
We will import our `RoomConnectionManager` and add endpoints to serve the multiplayer room HTML page and handle WebSockets connections.

#### [MODIFY] `app/main.py`
We will append these routes and inject static folder checks:
- `GET /room/{room_id}`: Serves the multiplayer room page.
- `WS /ws/room/{room_id}`: WebSocket endpoint that accepts connection, listens for client-side LLM outputs, renders panels on-demand using Pillow, and broadcasts the panel URL to all room participants.

```python
# Import Room Connection Manager
from app.services.room_manager import RoomConnectionManager

# Initialize connection manager
room_manager = RoomConnectionManager()

# ...

@app.get("/room/{room_id}")
async def get_room(room_id: str):
    """
    Serves the room.html multiplayer chat screen.
    """
    room_html_path = static_dir / "room.html"
    if room_html_path.exists():
        return FileResponse(str(room_html_path))
    raise HTTPException(status_code=404, detail="room.html template not found")

@app.websocket("/ws/room/{room_id}")
async def room_websocket(websocket: WebSocket, room_id: str):
    """
    WebSocket endpoint for real-time room communication and panel synchronization.
    """
    await room_manager.connect(websocket, room_id)
    try:
        while True:
            # Receive message from user
            data = await websocket.receive_json()
            
            msg_type = data.get("type")
            sender = data.get("sender", "Anonymous")
            
            if msg_type == "chat_message":
                text = data.get("text", "")
                character_name = data.get("character_name", "hero")
                detected_emotion = data.get("detected_emotion", "neutral")
                
                # Form Comic Panel Structured payload
                panel_data = ComicPanelOutput(
                    character_name=character_name,
                    detected_emotion=detected_emotion,
                    formatted_dialogue=text
                )
                
                # Render panel using the configured image renderer (Pillow or Mock)
                image_bytes = await renderer_provider.render_panel(panel_data)
                
                # Save locally in static dir
                import time
                filename = f"room_{room_id}_{int(time.time())}.jpg"
                file_path = static_dir / "generated" / filename
                with open(file_path, "wb") as f:
                    f.write(image_bytes)
                
                # Broadcast the message and rendered image URL to all participants
                broadcast_payload = {
                    "type": "new_panel",
                    "sender": sender,
                    "dialogue": text,
                    "character": character_name,
                    "emotion": detected_emotion,
                    "image_url": f"/static/generated/{filename}"
                }
                await room_manager.broadcast(broadcast_payload, room_id)
                
    except WebSocketDisconnect:
        room_manager.disconnect(websocket, room_id)
    except Exception as e:
        logger.error(f"WebSocket error in room {room_id}: {e}")
        room_manager.disconnect(websocket, room_id)
```

---

### 3. Multiplayer Frontend Clients (`app/static/index.html` & `app/static/room.html`)
- We will modify `app/static/index.html` to add a "Create Room" input panel where users can type a room name, create a room, and generate shared invite links.
- We will create a new HTML file `app/static/room.html` containing:
  - Username prompt on join.
  - Connection indicator for the WebSocket server.
  - Active detection indicator for Chrome Canary's `window.ai` / `ai.assistant`.
  - Chat window displaying text and generated comic panels for all users.
  - Local LLM prompt template that formats user messages into structured character + emotion + dialogue states using client-side Gemini Nano (falling back to a smart client-side mock if Gemini Nano is unavailable).

---

## 🧪 Verification Plan

### Automated Tests
We will add a WebSocket integration test in [tests/test_pipeline.py](file:///Users/neilghosh/dev/comic-chat/tests/test_pipeline.py) using FastAPI's `websocket_connect` test client wrapper to verify:
1. Connecting multiple WebSocket users to the same `room_id`.
2. Sending a structured JSON panel trigger from one client.
3. Asserting that all connected clients receive the broadcasted message containing the compiled `/static/generated/` URL.

### Manual Verification
1. Start the server using: `PYTHONPATH=. venv/bin/uvicorn app.main:app --reload`
2. Open `http://localhost:8000/` and click **Create Multiplayer Room**.
3. Copy the room link (e.g., `http://localhost:8000/room/testing-123`).
4. Open the link in **two separate browser windows** (or two different browsers/profiles).
5. Set different usernames in each window.
6. Type a message in Window A. Observe that Window B receives the rendered comic panel instantly.
