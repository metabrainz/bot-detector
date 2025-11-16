# Internal HTTP Server API

When the `--http-server` flag is used (e.g., `--http-server "127.0.0.1:8080"`), the bot-detector starts an internal web server to provide live metrics and access to the current configuration.

This API is intended for monitoring and administrative purposes.

## Endpoints

### **`/` or `/stats`**

*   **Method:** `GET`
*   **Content-Type:** `text/html; charset=utf-8`
*   **Description:** Displays a comprehensive HTML report of the application's real-time metrics. This is the main dashboard for monitoring activity. The report includes:
    *   General processing statistics (lines processed, valid hits, errors).
    *   Actor statistics (good actors skipped, actors cleaned).
    *   Chain and action statistics.
    *   Per-chain metrics (hits, completions, resets).

### **`/stats/steps`**

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Provides a plain text list of all behavioral chain steps and the number of times each has been executed. This is useful for debugging chain performance and identifying which rules are matching most frequently. The list is sorted by execution count in descending order.

### **`/config`**

*   **Method:** `GET`
*   **Content-Type:** `text/yaml; charset=utf-8`
*   **Description:** Returns the raw YAML content of the main configuration file as it was last loaded by the application. This allows you to inspect the exact configuration that is currently active, which is especially useful after a hot-reload.
*   **Responses:**
    *   `200 OK`: Successfully returns the YAML configuration.
    *   `500 Internal Server Error`: If the server fails to retrieve or marshal the configuration content.

### **`/config/archive`**

*   **Method:** `GET`
*   **Content-Type:** `application/gzip`
*   **Description:** Downloads a `.tar.gz` archive containing the active `config.yaml` and all of its file-based dependencies (e.g., files referenced with `file:`). This provides a complete, portable snapshot of a working configuration, which can be used for backups or for migrating the exact same ruleset to another bot-detector instance. The archive preserves the relative directory structure of the dependencies.
*   **Responses:**
    *   `200 OK`: Successfully begins streaming the gzipped tar archive.
    *   `500 Internal Server Error`: If the server fails to access the configuration or its dependencies to create the archive.
