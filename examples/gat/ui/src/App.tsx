import { useEffect, useState } from 'react'
import { graphql } from './gql'
import { client } from './client'

// graphql() is the codegen-generated tagged template. The string
// literal is parsed at codegen time; the return value is a typed
// DocumentNode whose result type is inferred from the schema. Add
// or change a field here, run `pnpm gen`, and TypeScript will catch
// every UI mismatch on the next save.
const ListProjectsDoc = graphql(/* GraphQL */ `
  query ListProjects {
    projects {
      listProjects {
        projects {
          id
          name
          tags
        }
      }
    }
  }
`)

// Field selection in action: this query only asks for id+name.
// Whatever extra fields the schema grows, this resolver only
// fetches what it needs.
const GetProjectDoc = graphql(/* GraphQL */ `
  query GetProject($id: String!) {
    projects {
      getProject(id: $id) {
        id
        name
      }
    }
  }
`)

type Project = {
  id: string
  name: string
  tags?: readonly string[] | null
}

export function App() {
  const [list, setList] = useState<readonly Project[]>([])
  const [selected, setSelected] = useState<{ id: string; name: string } | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    client.request(ListProjectsDoc)
      .then(res => {
        setList(res.projects?.listProjects?.projects ?? [])
      })
      .catch(e => setError(String(e)))
  }, [])

  function select(id: string) {
    client.request(GetProjectDoc, { id })
      .then(res => setSelected(res.projects?.getProject ?? null))
      .catch(e => setError(String(e)))
  }

  return (
    <div style={{ fontFamily: 'system-ui', maxWidth: 640, margin: '2rem auto', padding: '0 1rem' }}>
      <h1>gat example</h1>
      <p style={{ color: '#666' }}>
        One huma source of truth, three typed surfaces:
        REST (huma native), GraphQL (gat), gRPC (gat + connect-go).
        This UI uses graphql-codegen + typed-document-node off
        <code> /api/schema/graphql</code>.
      </p>

      {error && <pre style={{ color: 'crimson' }}>{error}</pre>}

      <h2>Projects (list)</h2>
      <ul>
        {list.map(p => (
          <li key={p.id}>
            <button onClick={() => select(p.id)}>{p.name}</button>
            {p.tags && p.tags.length > 0 && (
              <span style={{ color: '#888', marginLeft: 8 }}>
                [{p.tags.join(', ')}]
              </span>
            )}
          </li>
        ))}
      </ul>

      {selected && (
        <>
          <h2>Detail (field-selected: id, name only)</h2>
          <pre>{JSON.stringify(selected, null, 2)}</pre>
        </>
      )}
    </div>
  )
}
