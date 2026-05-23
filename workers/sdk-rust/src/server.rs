use std::error::Error;
use axum::Router;

pub type ServeError = Box<dyn Error + Send + Sync>;

#[async_trait::async_trait]
pub trait Handler: Send + Sync + 'static {
    async fn handle(&self, tool: &str, params: serde_json::Value) -> Result<serde_json::Value, ServeError>;
}

pub async fn serve(port: u16, _handler: impl Handler) -> Result<(), ServeError> {
    let app = Router::new();
    let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{port}")).await?;
    axum::serve(listener, app).await?;
    Ok(())
}

#[allow(dead_code)]
fn main() {
    // This is a stub binary for the crucible-server
}
