export default function Login() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50">
      <div className="rounded-xl border bg-white p-10 text-center shadow-sm">
        <h1 className="mb-2 text-2xl font-semibold">DevPod</h1>
        <p className="mb-6 text-sm text-slate-500">Remote development environments</p>
        <a
          href="/auth/login"
          className="rounded-lg bg-orange-600 px-6 py-2 font-medium text-white hover:bg-orange-700"
        >
          Sign in with GitLab
        </a>
      </div>
    </main>
  );
}
