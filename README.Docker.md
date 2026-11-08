### Building and running your application

When you're ready, start your application by running:
`docker compose up --build`.

Your application will be available at http://localhost:3334.

### Deploying your application to the cloud

First, build your image, e.g.: `docker build -t myapp .`.
If your cloud uses a different CPU architecture than your development
machine (e.g., you are on a Mac M1 and your cloud provider is amd64),
you'll want to build the image for that platform, e.g.:
`docker build --platform=linux/amd64 -t myapp .`.

Then, push it to your registry, e.g. `docker push myregistry.com/myapp`.

Consult Docker's [getting started](https://docs.docker.com/go/get-started-sharing/)
docs for more detail on building and pushing.

### References
* [Docker's Go guide](https://docs.docker.com/language/golang/)
### Search (NIP-50)

The relay serves NIP-50 search via PostgreSQL full-text search. On first start
it builds a GIN index over existing events in the background
(`CREATE INDEX CONCURRENTLY`, the table stays writable). See
[SEARCH.md](SEARCH.md) for query semantics, the `SEARCH_ENABLED`,
`SEARCH_INDEX_BUILD` and `SEARCH_TIMEOUT` settings, migration/rollback and
operational cost.
