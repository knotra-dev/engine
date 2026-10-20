use super::*;
use std::{
    io::{Read, Write},
    net::TcpListener,
};
use tauri::ipc::InvokeResponseBody;
fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
}

fn fixture<F>(count: usize, handle: F) -> (Session, std::thread::JoinHandle<()>)
where
    F: Fn(usize, String) -> (u16, &'static str, Vec<u8>) + Send + 'static,
{
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let task = std::thread::spawn(move || {
        for index in 0..count {
            let (mut socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let mut bytes = Vec::new();
            let mut length = None;
            loop {
                let mut buffer = [0; 4096];
                let size = socket.read(&mut buffer).unwrap();
                if size == 0 {
                    break;
                }
                bytes.extend_from_slice(&buffer[..size]);
                if let Some(end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                    let headers = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                    length = Some(
                        end + 4
                            + headers
                                .lines()
                                .find_map(|line| {
                                    line.strip_prefix("content-length: ")
                                        .and_then(|value| value.parse::<usize>().ok())
                                })
                                .unwrap_or(0),
                    );
                }
                if length.is_some_and(|length| bytes.len() >= length) {
                    break;
                }
            }
            let (status, kind, body) = handle(index, String::from_utf8(bytes).unwrap());
            let headers = format!(
                "HTTP/1.1 {status} Test\r\nContent-Type: {kind}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            );
            socket.write_all(headers.as_bytes()).unwrap();
            socket.write_all(&body).unwrap();
        }
    });
    (
        Session {
            base: Url::parse(&format!("http://{address}/")).unwrap(),
            key: "fixture".into(),
            token: Some("memory-only".into()),
            client: Client::builder()
                .redirect(reqwest::redirect::Policy::none())
                .build()
                .unwrap(),
        },
        task,
    )
}

#[test]
fn lost_receipt_reuses_exact_key_payload_and_only_one_logical_operation() {
    runtime().block_on(async {
        let deliveries = Arc::new(Mutex::new(Vec::<String>::new()));
        let recorded = deliveries.clone();
        let (session, task) = fixture(2, move |index, request| {
            recorded.lock().unwrap().push(request);
            (
                200,
                "application/json",
                if index == 0 {
                    b"{".to_vec()
                } else {
                    br#"{"run":{"id":"r1"}}"#.to_vec()
                },
            )
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let call = Call::Start {
            definition_id: "d1".into(),
            profile: "p1".into(),
            inputs: json!({"a":null}),
            artifacts: json!({}),
            operation_id: "stable-operation".into(),
        };
        assert!(execute(&store, &session, &call).await.is_err());
        assert_eq!(
            store.snapshot("fixture").unwrap()["pending"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            execute(&store, &session, &call).await.unwrap()["run"]["id"],
            "r1"
        );
        assert_eq!(
            execute(&store, &session, &call).await.unwrap()["run"]["id"],
            "r1"
        );
        assert!(store.snapshot("fixture").unwrap()["pending"]
            .as_array()
            .unwrap()
            .is_empty());
        task.join().unwrap();
        let sent = deliveries.lock().unwrap();
        assert_eq!(sent[0], sent[1]);
        assert!(sent[0]
            .to_lowercase()
            .contains("idempotency-key: stable-operation"));
        assert!(sent[0]
            .to_lowercase()
            .contains("authorization: bearer memory-only"));
        assert!(!store
            .snapshot("fixture")
            .unwrap()
            .to_string()
            .contains("memory-only"));
    });
}

#[test]
fn definitive_schema_rejection_keeps_request_retriable_with_new_operation() {
    runtime().block_on(async {
        let (session, task) = fixture(1, |_, request| {
            assert!(request.starts_with("POST /v1/requests/request-A/response"));
            (
                422,
                "application/json",
                br#"{"code":"OUTPUT_INVALID","message":"Invalid response","diagnostics":[]}"#
                    .to_vec(),
            )
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let call = Call::Respond {
            request_id: "request-A".into(),
            outputs: json!({"feedback":false}),
            operation_id: "response-1".into(),
        };
        assert_eq!(
            execute(&store, &session, &call).await.unwrap_err().status,
            Some(422)
        );
        assert_eq!(
            execute(&store, &session, &call).await.unwrap_err().status,
            Some(422)
        );
        assert!(store.snapshot("fixture").unwrap()["pending"]
            .as_array()
            .unwrap()
            .is_empty());
        task.join().unwrap();
        assert!(store
            .prepare(
                "fixture",
                "response-2",
                &json!({"op":"respond","requestId":"request-A","outputs":{"feedback":"valid"}}),
                || Ok(())
            )
            .unwrap()
            .is_none());
    });
}

#[test]
fn sse_resume_persists_before_delivery_and_deduplicates_replay() {
    runtime().block_on(async {
        let (session, task) = fixture(2, |index, request| {
            if index == 1 {
                assert!(request.to_lowercase().contains("last-event-id: e1"));
            }
            let one = "id: e1\ndata: {\"id\":\"e1\",\"runId\":\"r1\",\"message\":\"one\"}\n\n";
            let two = "id: e2\ndata: {\"id\":\"e2\",\"runId\":\"r1\",\"message\":\"two\"}\n\n";
            (
                200,
                "text/event-stream",
                if index == 0 {
                    format!("{one}{one}")
                } else {
                    format!("{one}{two}")
                }
                .into_bytes(),
            )
        });
        let store = Arc::new(store::Store::open(std::path::Path::new(":memory:")).unwrap());
        let received = Arc::new(Mutex::new(Vec::new()));
        let recorded = received.clone();
        let committed = store.clone();
        let channel = Channel::new(move |body| {
            if let InvokeResponseBody::Json(text) = body {
                let value: Value = serde_json::from_str(&text).unwrap();
                if value["type"] == "event" {
                    assert_eq!(
                        committed.cursor("fixture", "r1").unwrap().as_deref(),
                        value["event"]["id"].as_str()
                    );
                    recorded.lock().unwrap().push(value["event"].clone());
                }
            }
            Ok(())
        });
        stream(&session, &store, "r1", &channel).await.unwrap();
        stream(&session, &store, "r1", &channel).await.unwrap();
        task.join().unwrap();
        assert_eq!(received.lock().unwrap().len(), 2);
        assert_eq!(store.events("fixture", "r1").unwrap().len(), 2);
    });
}

#[test]
fn downloaded_binary_is_hash_checked_before_leaving_native_backend() {
    runtime().block_on(async {
        for corrupt in [false, true] {
            let bytes = vec![0, 255, 128, 10];
            let hash = if corrupt {
                "0".repeat(64)
            } else {
                format!("{:x}", Sha256::digest(&bytes))
            };
            let (session, task) = fixture(2, move |index, request| {
                if index == 0 {
                    assert!(request.starts_with("GET /v1/artifacts/a1 "));
                    (
                        200,
                        "application/json",
                        format!(
                            r#"{{"artifact":{{"id":"a1","name":"input.bin","mediaType":"application/octet-stream","size":4e0,"sha256":"{hash}","origin":{{}}}}}}"#
                        )
                        .into_bytes(),
                    )
                } else {
                    assert!(request.starts_with("GET /v1/artifacts/a1/content "));
                    (200, "application/octet-stream", bytes.clone())
                }
            });
            let result = download(&session, "a1").await;
            task.join().unwrap();
            if corrupt {
                assert_eq!(result.unwrap_err().code, "integrity");
            } else {
                assert_eq!(
                    STANDARD
                        .decode(result.unwrap()["content"].as_str().unwrap())
                        .unwrap(),
                    [0, 255, 128, 10]
                );
            }
        }
    });
}

#[test]
fn mismatched_download_metadata_is_rejected_before_fetching_content() {
    runtime().block_on(async {
        let (session, task) = fixture(1, |_, request| {
            assert!(request.starts_with("GET /v1/artifacts/a1 "));
            (
                200,
                "application/json",
                json!({"artifact": {
                    "id": "another-artifact",
                    "name": "input.bin",
                    "mediaType": "application/octet-stream",
                    "size": 4,
                    "sha256": "0".repeat(64),
                    "origin": {}
                }})
                .to_string()
                .into_bytes(),
            )
        });
        let error = download(&session, "a1").await.unwrap_err();
        task.join().unwrap();
        assert_eq!(error.code, "protocol");
        assert_eq!(
            error.message,
            "Artifact identity does not match the request."
        );
    });
}

#[test]
fn pagination_preserves_structured_engine_rejections() {
    runtime().block_on(async {
        let (session, task) = fixture(1, |_, request| {
            assert!(request.starts_with("GET /v1/runs?cursor=page-2&preview=javascript "));
            (
                401,
                "application/json",
                br#"{"code":"UNAUTHENTICATED","message":"Access token expired.","diagnostics":[{"code":"TOKEN_EXPIRED"}]}"#.to_vec(),
            )
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let error = execute(
            &store,
            &session,
            &Call::Runs {
                cursor: Some("page-2".into()),
            },
        )
        .await
        .unwrap_err();
        task.join().unwrap();
        assert_eq!(error.code, "UNAUTHENTICATED");
        assert_eq!(error.message, "Access token expired.");
        assert_eq!(error.status, Some(401));
        assert_eq!(error.diagnostics[0]["code"], "TOKEN_EXPIRED");
        assert!(store.snapshot("fixture").unwrap()["cache"]
            .as_object()
            .unwrap()
            .is_empty());
    });
}

#[test]
fn instance_history_is_scoped_paginated_and_not_added_to_the_event_cursor() {
    runtime().block_on(async {
        let (session, task) = fixture(2, |index, request| {
            assert!(request.starts_with("GET /v1/runs/r1/history?cursor=42&instanceId=n%2Fa "));
            (200, "application/json", json!({
                "items": [{"id":"43", "runId":"r1", "instanceId":if index == 0 { "n/a" } else { "another-node" }}],
                "nextCursor":null,
            }).to_string().into_bytes())
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let call = Call::History { run_id:"r1".into(), instance_id:Some("n/a".into()), cursor:Some("42".into()) };
        let page = execute(&store, &session, &call).await.unwrap();
        assert_eq!(page["items"][0]["id"], "43");
        assert!(store.cursor("fixture", "r1").unwrap().is_none());
        assert!(store.snapshot("fixture").unwrap()["cache"].as_object().unwrap().is_empty());
        assert_eq!(execute(&store, &session, &call).await.unwrap_err().code, "protocol");
        task.join().unwrap();
    });
}

#[test]
fn unsafe_event_numbers_do_not_advance_the_durable_cursor() {
    runtime().block_on(async {
        let (session, task) = fixture(1, |_, request| {
            assert!(request.to_lowercase().contains("last-event-id: e1"));
            (
                200,
                "text/event-stream",
                b"id: e2\ndata: {\"id\":\"e2\",\"runId\":\"r1\",\"data\":{\"value\":9007199254740993}}\n\n".to_vec(),
            )
        });
        let store = Arc::new(store::Store::open(std::path::Path::new(":memory:")).unwrap());
        store
            .event("fixture", "r1", "e1", &json!({"id":"e1","runId":"r1"}))
            .unwrap();
        let received = Arc::new(Mutex::new(Vec::new()));
        let recorded = received.clone();
        let channel = Channel::new(move |body| {
            if let InvokeResponseBody::Json(text) = body {
                let value: Value = serde_json::from_str(&text).unwrap();
                if value["type"] == "event" {
                    recorded.lock().unwrap().push(value);
                }
            }
            Ok(())
        });
        let error = stream(&session, &store, "r1", &channel).await.unwrap_err();
        task.join().unwrap();
        assert_eq!(error.code, "precision");
        assert!(received.lock().unwrap().is_empty());
        assert_eq!(store.cursor("fixture", "r1").unwrap().as_deref(), Some("e1"));
        assert_eq!(store.events("fixture", "r1").unwrap().len(), 1);
    });
}

#[test]
fn malformed_read_page_does_not_replace_the_last_valid_cache() {
    runtime().block_on(async {
        let page = json!({
            "items": [{
                "id": "r1",
                "definitionId": "d1",
                "title": "A saved run",
                "status": "running",
                "createdAt": "2026-10-04T00:00:00Z",
                "updatedAt": "2026-10-04T00:00:00Z",
                "profile": "local",
                "package": {
                    "entrypoint": "pipeline.yaml",
                    "source": "kind: Pipeline",
                    "files": [{
                        "path": "pipeline.yaml",
                        "content": STANDARD.encode("kind: Pipeline")
                    }]
                },
                "inputs": {},
                "inputArtifacts": {},
                "outputs": {},
                "artifacts": [],
                "instances": [],
                "diagnostics": [],
                "availableActions": ["cancel"]
            }],
            "nextCursor": null
        });
        let expected = page.clone();
        let (session, task) = fixture(2, move |index, request| {
            assert!(request.starts_with("GET /v1/runs?preview=javascript "));
            let response = if index == 0 {
                page.clone()
            } else {
                json!({"items":[{"id":"r1","status":"failed"}],"nextCursor":null})
            };
            (200, "application/json", response.to_string().into_bytes())
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let call = Call::Runs { cursor: None };
        assert_eq!(execute(&store, &session, &call).await.unwrap(), expected);
        let before = store.snapshot("fixture").unwrap();
        assert_eq!(
            execute(&store, &session, &call).await.unwrap_err().code,
            "protocol"
        );
        assert_eq!(store.snapshot("fixture").unwrap(), before);
        task.join().unwrap();
    });
}

#[test]
fn javascript_previews_keep_refresh_and_admission_receipts_readable() {
    runtime().block_on(async {
        let run = json!({
            "id":"r1", "definitionId":"d1", "title":"Exact integers", "status":"succeeded",
            "createdAt":"2026-10-04T00:00:00Z", "updatedAt":"2026-10-04T00:00:00Z",
            "profile":"local", "package":{"entrypoint":"pipeline.yaml", "source":"", "files":[{"path":"pipeline.yaml","content":""}]},
            "inputs":{"safe":"kept"}, "inputArtifacts":{}, "outputs":{}, "artifacts":[],
            "instances":[], "diagnostics":[], "availableActions":[], "dataTruncated":true
        });
        let expected = run.clone();
        let (session, task) = fixture(4, move |index, request| {
            let response = match index {
                0 => {
                    assert!(request.starts_with("GET /v1/runs?preview=javascript "));
                    json!({"items":[run.clone()], "nextCursor":null})
                }
                1 => {
                    assert!(request.starts_with("GET /v1/runs/r1?preview=javascript "));
                    json!({"run":run.clone()})
                }
                2 => {
                    assert!(request.starts_with("GET /v1/requests?preview=javascript "));
                    json!({"items":[{
                        "id":"h1", "runId":"r1", "instanceId":"n1", "attemptId":"n1.a1",
                        "status":"open", "prompt":"Review", "createdAt":"2026-10-04T00:00:00Z",
                        "deadline":"2026-10-04T00:00:00Z", "inputs":{"values":{}, "artifacts":{}},
                        "responseSchema":null, "responseSchemaJSON":"{\"const\":9223372036854775807}",
                        "dataTruncated":true
                    }], "nextCursor":null})
                }
                _ => {
                    assert!(request.starts_with("POST /v1/runs?preview=javascript "));
                    json!({"run":run.clone()})
                }
            };
            (if index == 3 { 201 } else { 200 }, "application/json", serde_json::to_vec(&response).unwrap())
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        assert_eq!(execute(&store, &session, &Call::Runs { cursor:None }).await.unwrap()["items"][0], expected);
        assert_eq!(execute(&store, &session, &Call::Run { run_id:"r1".into() }).await.unwrap()["run"], expected);
        let requests = execute(&store, &session, &Call::Requests { cursor:None }).await.unwrap();
        assert!(requests["items"][0]["responseSchema"].is_null());
        assert_eq!(requests["items"][0]["responseSchemaJSON"], "{\"const\":9223372036854775807}");
        let start = Call::Start { definition_id:"d1".into(), profile:"local".into(), inputs:json!({}), artifacts:json!({}), operation_id:"start-int64".into() };
        assert_eq!(execute(&store, &session, &start).await.unwrap()["run"], expected);
        assert_eq!(execute(&store, &session, &start).await.unwrap()["run"], expected);
        let unsafe_start = Call::Start { definition_id:"d1".into(), profile:"local".into(), inputs:json!({"exact":9007199254740993u64}), artifacts:json!({}), operation_id:"unsafe-start".into() };
        assert_eq!(execute(&store, &session, &unsafe_start).await.unwrap_err().code, "precision");
        assert!(store.snapshot("fixture").unwrap()["pending"].as_array().unwrap().is_empty());
        task.join().unwrap();
    });
}

#[test]
fn retained_unsafe_commands_reconcile_without_admitting_new_unsafe_commands() {
    runtime().block_on(async {
        let (session, task) = fixture(2, |index, request| {
            let body: Value =
                serde_json::from_str(request.split("\r\n\r\n").nth(1).unwrap()).unwrap();
            if index == 0 {
                assert!(request.starts_with("POST /v1/runs?preview=javascript "));
                assert!(request
                    .to_lowercase()
                    .contains("idempotency-key: retained-start"));
                assert_eq!(body["inputs"]["exact"], json!(9007199254740992u64));
                (
                    201,
                    "application/json",
                    br#"{"run":{"id":"recovered-run","dataTruncated":true}}"#.to_vec(),
                )
            } else {
                assert!(request.starts_with("POST /v1/requests/h1/response "));
                assert!(request
                    .to_lowercase()
                    .contains("idempotency-key: retained-response"));
                assert_eq!(body["outputs"]["exact"], json!(9007199254740992u64));
                (
                    200,
                    "application/json",
                    br#"{"accepted":true,"requestId":"h1"}"#.to_vec(),
                )
            }
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let start = |id: &str, exact: u64| Call::Start {
            definition_id: "d1".into(),
            profile: "local".into(),
            inputs: json!({"exact":exact}),
            artifacts: json!({}),
            operation_id: id.into(),
        };
        let pending_start = start("retained-start", 9007199254740992);
        let pending_response = Call::Respond {
            request_id: "h1".into(),
            outputs: json!({"exact":9007199254740992u64}),
            operation_id: "retained-response".into(),
        };
        let completed_start = start("completed-start", 9007199254740992);
        for call in [&pending_start, &pending_response, &completed_start] {
            store
                .prepare(
                    "fixture",
                    call.operation().unwrap(),
                    &serde_json::to_value(call).unwrap(),
                    || Ok(()),
                )
                .unwrap();
        }
        let completed = json!({"run":{"id":"completed-run"}});
        store
            .complete("fixture", "completed-start", &completed)
            .unwrap();
        assert_eq!(
            execute(&store, &session, &completed_start).await.unwrap(),
            completed
        );
        assert_eq!(
            execute(&store, &session, &pending_start).await.unwrap()["run"]["id"],
            "recovered-run"
        );
        assert_eq!(
            execute(&store, &session, &pending_response).await.unwrap()["accepted"],
            true
        );
        assert_eq!(
            execute(&store, &session, &pending_start).await.unwrap()["run"]["id"],
            "recovered-run"
        );
        assert_eq!(
            execute(&store, &session, &start("retained-start", 9007199254740994))
                .await
                .unwrap_err()
                .code,
            "conflict"
        );
        assert_eq!(
            execute(&store, &session, &start("new-start", 9007199254740992))
                .await
                .unwrap_err()
                .code,
            "precision"
        );
        assert!(store.snapshot("fixture").unwrap()["pending"]
            .as_array()
            .unwrap()
            .is_empty());
        task.join().unwrap();
    });
}
