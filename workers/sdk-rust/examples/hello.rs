use crucible_sdk::{Request, Response, WorkerError};

struct HelloHandler;

#[async_trait::async_trait]
impl crucible_sdk::Handler for HelloHandler {
    async fn handle(&self, req: Request) -> Result<Response, WorkerError> {
        let name = req.payload.as_str().unwrap_or("world");
        Ok(Response {
            payload: serde_json::json!({"message": format!("Hello, {name}!")}),
            billable_units: 1,
            units_label: Some("request".to_string()),
        })
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    tracing_subscriber::fmt::init();
    let port = std::env::var("PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .unwrap_or(8081);
    tracing::info!(port, "starting hello worker");
    crucible_sdk::server::serve(port, HelloHandler).await
}
